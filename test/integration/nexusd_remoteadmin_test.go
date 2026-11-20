// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

// WI 5.2: nexusd remote administration on real systemd, the feature's T1
// matrix (planning/nexusd-remote-admin/test-cases.md TC-RA-93 to TC-RA-103,
// TC-RA-E26, TC-RA-E28, TC-RA-ERR32 to TC-RA-ERR36, TC-RA-ERR39 and
// TC-RA-ERR44). Every case starts from a wiped host (raHost): install.sh
// writes the shipped default /etc/nexus/nexusd.yaml, sed enables the nexusd
// provider in place, nexusd.service starts under the exact shipped unit with
// no drop-in, `nexusd token create` mints the token ra (nexusd:read,
// nexusd:admin, host:read), and this binary runs as the nexus CLI over the
// pinned certificate. Cleanup waits for every nexus-op-* unit to end and
// puts back the harness install.
//
// The cases that need the reload window held use a deterministic gate on
// the helper's own unit, never a timing race (architecture WI 5.2, A-34):
// held arms a test-only prefix drop-in, nexus-op-.service.d/ra-hold.conf in
// /run/systemd/system, whose ExecStartPre= creates /run/nexus-ra-hold/reached
// and waits for /run/nexus-ra-hold/open. systemd applies a "<prefix>-.service.d"
// directory to every unit whose name starts with that prefix, transient ones
// included; nexusd.service has no dash, so it gets no drop-in and
// requireExactUnit still passes. Nothing is compiled into the binary for
// this. Every held event is a file state read as root (sha256, stat,
// existence), never the API, which cannot answer while nexusd is stopped,
// and every wait is bounded and names its event.
//
// Expected values come from the files, reload-status.json, systemctl, the
// journal and the audit log, never only from Nexus. The NFR-2 figures are
// logged on lines starting with NFR for docs/verification/measurements.md.
// Nothing here names any host but 127.0.0.1 (CON-1).

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/reloadstatus"
)

const (
	raBackup    = nexusdConfig + ".bak"
	raTokens    = nexusdState + "/tokens.json"
	raStateDB   = nexusdState + "/state.db"
	raTokensAt  = "/v1/providers/nexusd/tokens"
	raConfigAt  = "/v1/providers/nexusd/config/"
	raUpdateAt  = smActionsAt + "update-config"
	raCommented = "/harness/fixtures/nexusd-commented.yaml"

	// The hold seam (architecture WI 5.2, A-34).
	raHoldDir      = "/run/nexus-ra-hold"
	raHoldReached  = raHoldDir + "/reached"
	raHoldOpen     = raHoldDir + "/open"
	raHoldDropInD  = "/run/systemd/system/" + detach.UnitPrefix + ".service.d"
	raHoldDropIn   = raHoldDropInD + "/ra-hold.conf"
	raHoldDropInFn = "ra-hold.conf"
	// raHoldConf is the drop-in: one ExecStartPre= that creates reached,
	// then waits for open, polling every 100 ms for at most 60 s (600
	// polls), and fails the unit if open never appears. $$ is a literal $
	// for systemd.
	raHoldConf = "[Service]\nExecStartPre=/bin/sh -c 'touch " + raHoldReached + "; i=0; while [ ! -e " + raHoldOpen +
		" ]; do i=$$((i+1)); [ $$i -le 600 ] || exit 1; sleep 0.1; done'\n"

	// Bounds from the architecture (test-cases.md Conventions, Timing
	// reference).
	raReachedBound  = 10 * time.Second // The 202 to reached
	raDigestPoll    = 20 * time.Millisecond
	raDigestBound   = 10 * time.Second // The gate's opening to the helper's write
	raRestoredBound = 20 * time.Second // The new digest to the first attempt's restore
	raExitBound     = 40 * time.Second // The new digest to exit.json: both 12 s attempts expired
	raMaxRuntime    = 54 * time.Second // The helper unit's MaxRuntime (A-5)
	raRestoreBudget = 30 * time.Second // NFR-2: a config set that restores, from tOpen to ended
	raSetBudget     = 15 * time.Second // NFR-2: a config set with a passing reload, plus the reload's own time
	raP95Budget     = 200 * time.Millisecond
	raOpBound       = 2 * time.Minute
)

// raRun is the T1 run's window: the first TestTCRA host's start, which
// TC-RA-103 scans from.
var raRun struct {
	sync.Mutex
	first time.Time
}

// raHost is one T1 case's host: the release tree installed by install.sh,
// its default configuration with the nexusd provider enabled, running
// under the exact shipped unit.
type raHost struct {
	*smHost
	nrestarts0 string // nexusd.service's NRestarts after the start
}

// newRAHost prepares raHost (test-cases.md Conventions, T1 host).
func newRAHost(t *testing.T) *raHost {
	t.Helper()
	// Config set runs its helper in a systemd unit outside nexusd.service:
	// systemd hosts only (OpenRC is WI 5.3).
	requireInit(t, platform.Systemd)
	raRun.Lock()
	if raRun.first.IsZero() {
		raRun.first = time.Now().Add(-time.Second)
	}
	raRun.Unlock()
	waitOpUnitsIdle(t, raMaxRuntime+time.Minute)
	freshHost(t)
	t.Cleanup(func() { waitOpUnitsIdle(t, raMaxRuntime+time.Minute) })
	runInstall(t)
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
	h := &raHost{smHost: &smHost{t: t}}
	h.pid0 = startExact(t)
	requireExactUnit(t)
	h.nrestarts0 = showUnit(t, nexusdUnit, "NRestarts")["NRestarts"]
	h.token = createToken(t, "ra", spec.ReadScope, spec.AdminScope, hostspec.ReadScope)
	h.fp = client.Fingerprint(readCert(t, filepath.Join(nexusdState, "tls", "cert.pem")).Raw)
	tc, err := client.PinnedTLS(h.fp)
	if err != nil {
		t.Fatal(err)
	}
	h.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	h.home = h.cliHome(h.token)
	t.Logf("T1 host: install.sh default config with the nexusd provider enabled; nexusd.service MainPID %d, "+
		"NRestarts %s, certificate %s", h.pid0, h.nrestarts0, h.fp)
	return h
}

func raRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// raYAMLValue is the decoded value at a dotted key path, nil when absent.
func raYAMLValue(t *testing.T, data []byte, path string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// raDo sends one request over the pinned connection with token and the
// headers, and returns the status, headers and body. A transport failure is
// fatal.
func (h *raHost) raDo(token, method, path, body string, hdr map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://"+exactAddr+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, b
}

// opIDs lists every operation ID, all pages.
func (h *raHost) opIDs() []string {
	h.t.Helper()
	var ids []string
	for _, b := range h.pages(h.token, "/v1/operations") {
		var l apitypes.List[apitypes.Operation]
		if err := json.Unmarshal(b, &l); err != nil {
			h.t.Fatal(err)
		}
		for _, op := range l.Items {
			ids = append(ids, op.ID)
		}
	}
	return ids
}

// pages GETs every page of a list endpoint, limit 1000, and returns the
// bodies.
func (h *raHost) pages(token, path string) [][]byte {
	h.t.Helper()
	var out [][]byte
	cursor := ""
	for range 100 {
		q := path + "?limit=1000"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		code, _, b := h.raDo(token, http.MethodGet, q, "", nil)
		if code != http.StatusOK {
			h.t.Fatalf("GET %s: %d %s", q, code, b)
		}
		out = append(out, b)
		var l struct {
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(b, &l); err != nil {
			h.t.Fatal(err)
		}
		if cursor = l.NextCursor; cursor == "" {
			return out
		}
	}
	h.t.Fatalf("GET %s: more than 100 pages", path)
	return nil
}

// raStatus is reload-status.json; ok is false when there is none yet.
func raStatus(t *testing.T) (reloadstatus.Status, bool) {
	t.Helper()
	st, ok, err := reloadstatus.Read(nexusdState)
	if err != nil {
		t.Fatalf("read %s: %v", reloadstatus.FileName, err)
	}
	return st, ok
}

// requirePC is the FR-8 post-condition on T1: once the operation has ended,
// the file's digest is the one reload-status.json reports applied, unless
// the outcome is config-restore-failed.
func requirePC(t *testing.T, op apitypes.Operation) {
	t.Helper()
	if problemSlug(op) == string(problem.ConfigRestoreFailed) {
		t.Logf("PC: %s ended config-restore-failed", op.ID)
		return
	}
	st, ok := raStatus(t)
	disk := sha(t, nexusdConfig)
	if !ok || !st.Applied || st.SHA256 != disk {
		t.Errorf("PC: %s ended %s, but reload-status.json %+v (present %t) does not report the disk's sha256 %s applied",
			op.ID, op.Status, st, ok, disk)
		return
	}
	t.Logf("PC: sha256(disk) %s == reload-status.json sha256, applied, seq %d", disk, st.Seq)
}

// requireUnitGone waits for operation id's transient unit to be collected.
func requireUnitGone(t *testing.T, id string) {
	t.Helper()
	unit := detach.Unit(id)
	waitUntil(t, 30*time.Second, unit+" to be collected", func() (bool, string) {
		p := showUnit(t, unit, "LoadState", "ActiveState")
		return p["LoadState"] == "not-found", fmt.Sprint(p)
	})
}

// opUnitsInJournal lists the nexus-op-* unit names the whole journal names
// since since.
var opUnitRE = regexp.MustCompile(regexp.QuoteMeta(detach.UnitPrefix) + `[A-Za-z0-9-]+\.service`)

func opUnitsInJournal(t *testing.T, since string) []string {
	t.Helper()
	out := mustSh(t, "journalctl", "--since", since, "--no-pager", "-o", "cat")
	var units []string
	for _, u := range opUnitRE.FindAllString(out, -1) {
		if !slices.Contains(units, u) {
			units = append(units, u)
		}
	}
	return units
}

// auditMark is the number of lines audit.jsonl has now.
func auditMark(t *testing.T) int {
	t.Helper()
	return len(auditSince(t, 0))
}

// raAudit is one audit.jsonl line.
type raAudit struct {
	ID        string            `json:"id"`
	Action    string            `json:"action"`
	Outcome   string            `json:"outcome"`
	Problem   string            `json:"problem"`
	RequestID string            `json:"requestId"`
	Params    map[string]any    `json:"params"`
	Facts     map[string]string `json:"facts"`
}

// auditSince decodes the audit.jsonl lines after the first mark lines.
func auditSince(t *testing.T, mark int) []raAudit {
	t.Helper()
	b, err := os.ReadFile(smAudit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []raAudit
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" || i < mark {
			continue
		}
		var r raAudit
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line %d %q: %v", i+1, line, err)
		}
		out = append(out, r)
	}
	return out
}

// configSet is one waited `nexus nexusdconfig update` with its operation.
func (h *raHost) configSet(changes ...string) (smRun, apitypes.Operation) {
	h.t.Helper()
	run := h.cli(append([]string{"nexusdconfig", "update", "--yes", "-o", "json"}, changes...)...)
	id := run.opID()
	if id == "" {
		h.t.Fatalf("nexusdconfig update %q started no operation (exit %d)", changes, run.code)
	}
	op := h.awaitOp(id, raOpBound)
	h.t.Logf("config set %q: operation %s %s, error %v, result %s", changes, id, op.Status, op.Error, op.Result)
	return run, op
}

// requireRefused checks a CLI run refused before any operation with slug,
// and returns its problem.
func requireRefused(t *testing.T, r smRun, slug problem.Slug, status int) map[string]any {
	t.Helper()
	typ, ok := problem.Lookup(slug)
	if !ok {
		t.Fatalf("no problem type %s", slug)
	}
	p := r.problem()
	if r.code != typ.Exit || smSlug(p) != string(slug) || int(asFloat(p["status"])) != status {
		t.Errorf("nexus %s: exit %d, problem %v; want exit %d with %s %d", strings.Join(r.args, " "), r.code, p,
			typ.Exit, slug, status)
	}
	if ops := r.ops(); len(ops) != 0 {
		t.Errorf("nexus %s printed operations %v, want none", strings.Join(r.args, " "), ops)
	}
	return p
}

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// problemErrors are a config-invalid problem's errors[].
func problemErrors(p map[string]any) []spec.ConfigError {
	b, err := json.Marshal(p["errors"])
	if err != nil {
		return nil
	}
	var errs []spec.ConfigError
	if json.Unmarshal(b, &errs) != nil {
		return nil
	}
	return errs
}

// jsonItems decodes a CLI run's -o json output: one array, or one object
// per line.
func jsonItems[T any](t *testing.T, stdout string) []T {
	t.Helper()
	s := strings.TrimSpace(stdout)
	var items []T
	if json.Unmarshal([]byte(s), &items) == nil {
		return items
	}
	items = nil
	for line := range strings.Lines(s) {
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("CLI output line %q: %v", line, err)
		}
		items = append(items, v)
	}
	return items
}

// raFileState is what a refused or no-op config set must leave as it was.
type raFileState struct {
	sha       string
	ino       uint64
	mtime     time.Time
	bak       bool
	bakSHA    string
	seq       uint64
	hasStatus bool
}

func fileState(t *testing.T) raFileState {
	t.Helper()
	fi, err := os.Stat(nexusdConfig)
	if err != nil {
		t.Fatal(err)
	}
	s := raFileState{sha: sha(t, nexusdConfig), ino: fi.Sys().(*syscall.Stat_t).Ino, mtime: fi.ModTime(),
		bak: exists(raBackup)}
	if s.bak {
		s.bakSHA = sha(t, raBackup)
	}
	st, ok := raStatus(t)
	s.seq, s.hasStatus = st.Seq, ok
	return s
}

func requireFileState(t *testing.T, when string, want raFileState) {
	t.Helper()
	if got := fileState(t); got != want {
		t.Errorf("%s: file %+v, want it unchanged: %+v", when, got, want)
	} else {
		t.Logf("%s: %s unchanged (sha256 %s, inode %d, mtime %s), .bak present %t, reload status seq %d", when,
			nexusdConfig, got.sha, got.ino, got.mtime.Format(time.RFC3339Nano), got.bak, got.seq)
	}
}

// raHold is the armed hold seam of one case.
type raHold struct {
	h       *raHost
	once    sync.Once
	stopped int       // The daemon PID sent SIGSTOP and not yet SIGCONT
	reached time.Time // When the test saw reached
}

// held arms the hold drop-in (test-cases.md Conventions, Held window) and
// registers its cleanup.
func (h *raHost) held() *raHold {
	t := h.t
	t.Helper()
	for _, p := range []string{raHoldDir, raHoldDropInD} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(raHoldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(raHoldDropInD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(raHoldDropIn, []byte(raHoldConf), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSh(t, detach.Systemctl, "daemon-reload")
	requireExactUnit(t)
	g := &raHold{h: h}
	t.Cleanup(g.release)
	t.Logf("hold armed: %s, nexusd.service still exact with no drop-in", raHoldDropIn)
	return g
}

// release is held's cleanup: open the gate, CONT the daemon, wait for every
// nexus-op-* unit, remove the seam and check the unit.
func (g *raHold) release() {
	t := g.h.t
	g.once.Do(func() {
		if exists(raHoldDir) {
			if err := os.WriteFile(raHoldOpen, nil, 0o644); err != nil {
				t.Errorf("release: create %s: %v", raHoldOpen, err)
			}
		}
		g.cont()
		if pid := unitPID(t); pid > 0 {
			if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
				t.Logf("release: SIGCONT %d: %v", pid, err)
			}
		}
		waitOpUnitsIdle(t, raMaxRuntime+30*time.Second)
		for _, p := range []string{raHoldDropInD, raHoldDir} {
			if err := os.RemoveAll(p); err != nil {
				t.Errorf("release: %v", err)
			}
		}
		mustSh(t, detach.Systemctl, "daemon-reload")
		requireExactUnit(t)
		t.Logf("hold released: %s removed, nexusd.service exact with no drop-in", raHoldDropIn)
	})
}

// gate is the gate sequence's steps 1 to 3: the config set answers 202 with
// its operation, the unit reaches the hold, and the drop-in applied to it.
func (g *raHold) gate(changes ...string) string {
	h, t := g.h, g.h.t
	t.Helper()
	run := h.cli(append([]string{"nexusdconfig", "update", "--yes", "--no-wait", "-o", "json", "--verbose"}, changes...)...)
	id := run.opID()
	if run.code != 0 || id == "" || !strings.Contains(run.stderr, "< 202 ") {
		t.Fatalf("nexusdconfig update --no-wait %q: exit %d, operation %q; want 202 with an operation", changes,
			run.code, id)
	}
	unit := detach.Unit(id)
	failHold := func(why string) {
		st, _ := sh(30*time.Second, detach.Systemctl, "status", "--no-pager", unit)
		t.Fatalf("hold drop-in did not apply to the transient unit %s: %s\nsystemctl status %s:\n%s", detach.UnitPrefix+id,
			why, unit, st)
	}
	deadline := time.Now().Add(raReachedBound)
	for !exists(raHoldReached) {
		if time.Now().After(deadline) {
			failHold(fmt.Sprintf("%s did not appear within %s of the 202", raHoldReached, raReachedBound))
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.reached = time.Now()
	p := showUnit(t, unit, "DropInPaths", "ActiveState", "SubState")
	if !slices.Contains(strings.Fields(p["DropInPaths"]), raHoldDropIn) {
		failHold(fmt.Sprintf("DropInPaths of %s is %q, want it to list %s", unit, p["DropInPaths"], raHoldDropIn))
	}
	t.Logf("gate: %s reached; %s DropInPaths=%s ActiveState=%s SubState=%s", id, unit, p["DropInPaths"],
		p["ActiveState"], p["SubState"])
	return id
}

// stop is the pre-helper action held: SIGSTOP nexusd's MainPID and see it
// stopped (state T).
func (g *raHold) stop() {
	t := g.h.t
	t.Helper()
	pid := unitPID(t)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP %d: %v", pid, err)
	}
	g.stopped = pid
	waitUntil(t, 5*time.Second, fmt.Sprintf("nexusd %d to be stopped", pid), func() (bool, string) {
		s := procState(pid)
		return s == "T", "state " + s
	})
	t.Logf("held: nexusd %d stopped (state T)", pid)
}

// cont sends SIGCONT to the daemon stop stopped, if any.
func (g *raHold) cont() {
	if g.stopped == 0 {
		return
	}
	pid := g.stopped
	g.stopped = 0
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		g.h.t.Errorf("SIGCONT %d: %v", pid, err)
		return
	}
	g.h.t.Logf("held: nexusd %d continued", pid)
}

// open creates open, so the helper runs, and returns that wall time, tOpen.
func (g *raHold) open() time.Time {
	t := g.h.t
	t.Helper()
	if err := os.WriteFile(raHoldOpen, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	t.Logf("gate open at %s", now.Format(time.RFC3339Nano))
	return now
}

// procState is the state letter of /proc/<pid>/stat.
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return err.Error()
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return "?"
	}
	return s[i+2 : i+3]
}

// waitDigest polls the file's sha256 every raDigestPoll until cond holds,
// at most bound; the failure names the event and attaches the unit's status
// and journal.
func waitDigest(t *testing.T, id, event string, bound time.Duration, cond func(sum string) bool) string {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		if b, err := os.ReadFile(nexusdConfig); err == nil {
			if sum := sha256Sum(b); cond(sum) {
				return sum
			}
		}
		if time.Now().After(deadline) {
			unit := detach.Unit(id)
			st, _ := sh(30*time.Second, detach.Systemctl, "status", "--no-pager", unit)
			j, _ := sh(30*time.Second, "journalctl", "-u", unit, "--no-pager", "-o", "cat")
			t.Fatalf("%s within %s for %s%s\nsystemctl status %s:\n%s\njournalctl -u %s:\n%s", event, bound,
				detach.UnitPrefix, id, unit, st, unit, j)
		}
		time.Sleep(raDigestPoll)
	}
}

// requireHelperWrote waits for the new digest: the helper wrote the edited
// file.
func requireHelperWrote(t *testing.T, id, orig string) {
	t.Helper()
	sum := waitDigest(t, id, "helper did not write while the daemon was held", raDigestBound,
		func(s string) bool { return s != orig })
	t.Logf("new digest %s (was %s)", sum, orig)
}

// raHeld is one held config set's outcome.
type raHeld struct {
	id    string
	op    apitypes.Operation
	orig  string
	tOpen time.Time
	took  time.Duration // ended − tOpen
}

func (r raHeld) ended(t *testing.T) time.Time {
	t.Helper()
	if r.op.Ended == nil {
		t.Fatalf("operation %s has no ended time", r.id)
	}
	return *r.op.Ended
}

// awaitHeld awaits the held operation and measures ended − tOpen.
func (g *raHold) awaitHeld(id, orig string, tOpen time.Time) raHeld {
	t := g.h.t
	t.Helper()
	op := g.h.awaitOp(id, raOpBound)
	r := raHeld{id: id, op: op, orig: orig, tOpen: tOpen}
	r.took = r.ended(t).Sub(tOpen)
	t.Logf("operation %s %s %s, error %v, result %s; ended − tOpen = %s", id, op.Action, op.Status, op.Error,
		op.Result, r.took.Round(time.Millisecond))
	return r
}

// heldRefusedRestore is TC-RA-ERR34's method: STOP, open, wait for the new
// digest, chmod 0644 so the daemon's ReadFile refuses it at stage load, CONT.
func (h *raHost) heldRefusedRestore(change string) raHeld {
	t := h.t
	t.Helper()
	orig := sha(t, nexusdConfig)
	g := h.held()
	id := g.gate(change)
	g.stop()
	tOpen := g.open()
	requireHelperWrote(t, id, orig)
	if err := os.Chmod(nexusdConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("chmod 0644 %s", nexusdConfig)
	g.cont()
	r := g.awaitHeld(id, orig, tOpen)
	g.release()
	return r
}

// heldUnconfirmedRestore is TC-RA-E26's method: STOP, open, wait for the
// new digest, wait for the original digest (the first attempt expired and
// the helper wrote the restore), CONT.
func (h *raHost) heldUnconfirmedRestore(change string) raHeld {
	t := h.t
	t.Helper()
	orig := sha(t, nexusdConfig)
	g := h.held()
	id := g.gate(change)
	g.stop()
	tOpen := g.open()
	requireHelperWrote(t, id, orig)
	waitDigest(t, id, "the helper did not restore the original digest after its first attempt", raRestoredBound,
		func(s string) bool { return s == orig })
	t.Logf("original digest back after %s: the first 12 s attempt expired", time.Since(tOpen).Round(time.Millisecond))
	g.cont()
	r := g.awaitHeld(id, orig, tOpen)
	g.release()
	return r
}

// requireOutcome checks a failed operation's slug.
func requireOutcome(t *testing.T, op apitypes.Operation, slug problem.Slug) {
	t.Helper()
	if op.Status != apitypes.StatusFailed || problemSlug(op) != string(slug) {
		t.Fatalf("operation %s %s %s (%v), want Failed %s", op.ID, op.Status, problemSlug(op), op.Error, slug)
	}
}

func requireSucceeded(t *testing.T, op apitypes.Operation) {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %s (%v), want Succeeded", op.ID, op.Status, op.Error)
	}
}

// requireRestored checks a restoring outcome: config-apply-failed with
// restored true, the disk back to orig at 0600 root:root, and the last
// reload status applied with orig's digest.
func requireRestored(t *testing.T, r raHeld) {
	t.Helper()
	requireOutcome(t, r.op, problem.ConfigApplyFailed)
	if v := r.op.Error.Extensions["restored"]; v != true {
		t.Errorf("restored = %v, want true", v)
	}
	if v := errExt(r.op, "backupPath"); v != raBackup {
		t.Errorf("backupPath = %q, want %s", v, raBackup)
	}
	if got := sha(t, nexusdConfig); got != r.orig {
		t.Errorf("sha256(%s) = %s, want the original %s", nexusdConfig, got, r.orig)
	}
	if uid, gid, perm := fileStat(t, nexusdConfig); uid != 0 || gid != 0 || perm != 0o600 {
		t.Errorf("%s is %d:%d %04o, want root:root 0600", nexusdConfig, uid, gid, perm)
	}
	st, ok := raStatus(t)
	if !ok || !st.Applied || st.SHA256 != r.orig {
		t.Errorf("reload-status.json %+v, want applied with sha256 %s", st, r.orig)
	}
	requirePC(t, r.op)
}

// nfrLine logs one NFR-2 figure for docs/verification/measurements.md.
func nfrLine(t *testing.T, what string, d, bound time.Duration) {
	t.Helper()
	t.Logf("NFR NFR-2 %s: %s (bound %s)", what, d.Round(time.Millisecond), bound)
	if d > bound {
		t.Errorf("NFR-2 %s took %s, over its %s bound", what, d.Round(time.Millisecond), bound)
	}
}

// raComments are the comments at one key path: the key's and its value's,
// merged, so a comment moving between the two still counts as attached.
type raComments struct{ Head, Line, Foot string }

// raCommentMap maps each key path, plus "<doc>" and "" for the document and
// root mapping, to its comments, as config.Edit's NFR-3 tests do
// (internal/config/edit_test.go commentMap).
func raCommentMap(t *testing.T, data []byte) map[string]raComments {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	m := map[string]raComments{}
	cat := func(a, b string) string {
		if a == "" || b == "" {
			return a + b
		}
		return a + "\n" + b
	}
	put := func(path string, ns ...*yaml.Node) {
		var c raComments
		for _, n := range ns {
			c.Head, c.Line, c.Foot = cat(c.Head, n.HeadComment), cat(c.Line, n.LineComment), cat(c.Foot, n.FootComment)
		}
		if c != (raComments{}) {
			m[path] = c
		}
	}
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		return a + "." + b
	}
	put("<doc>", &doc)
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				p := join(path, n.Content[i].Value)
				put(p, n.Content[i], n.Content[i+1])
				walk(n.Content[i+1], p)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				p := path + "[" + strconv.Itoa(i) + "]"
				put(p, c)
				walk(c, p)
			}
		}
	}
	if len(doc.Content) > 0 {
		put("", doc.Content[0])
		walk(doc.Content[0], "")
	}
	return m
}

// raNormalize is the documented re-encode normalization (A-10, TC-RA-51):
// blank lines dropped, runs of spaces before a comment collapsed.
func raNormalize(ls []string) string {
	spaces := regexp.MustCompile(` +#`)
	var out []string
	for _, l := range ls {
		if strings.TrimSpace(l) != "" {
			out = append(out, spaces.ReplaceAllString(l, " #"))
		}
	}
	return strings.Join(out, "\n")
}

// raJournalEntry is one journal entry: its time and message.
type raJournalEntry struct {
	at   time.Time
	unit string
	msg  string
}

// raJournal is the journal of the units since since, from -o json.
func raJournal(t *testing.T, since string, units ...string) []raJournalEntry {
	t.Helper()
	args := []string{"--since", since, "--no-pager", "-o", "json"}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	out := mustSh(t, "journalctl", args...)
	var es []raJournalEntry
	for line := range strings.Lines(out) {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		us, _ := strconv.ParseInt(fmt.Sprint(m["__REALTIME_TIMESTAMP"]), 10, 64)
		msg, _ := m["MESSAGE"].(string)
		unit, _ := m["_SYSTEMD_UNIT"].(string)
		if u, ok := m["UNIT"].(string); ok {
			unit = u
		}
		es = append(es, raJournalEntry{at: time.UnixMicro(us), unit: unit, msg: msg})
	}
	return es
}

var catHeaderRE = regexp.MustCompile(`^# (/\S+)$`)

// TC-RA-93 (WI 5.2; CON-1): the hardened unit is unchanged on the real host:
// byte-equal to deploy/nexusd.service, ProtectSystem=strict with no
// writable /etc, its own CapabilityBoundingSet and no drop-in.
func TestTCRA93_TheHardenedUnitIsUnchangedOnTheRealHost(t *testing.T) {
	newRAHost(t)
	installed, shipped := sha(t, installedUnit), sha(t, shippedUnit)
	t.Logf("sha256 %s %s; %s (the repository's deploy/nexusd.service) %s", installedUnit, installed, shippedUnit, shipped)
	if installed != shipped {
		t.Errorf("%s sha256 %s differs from deploy/nexusd.service's %s", installedUnit, installed, shipped)
	}
	p := showUnit(t, nexusdUnit, "ProtectSystem", "ReadWritePaths", "CapabilityBoundingSet", "ReadOnlyPaths")
	t.Logf("systemctl show nexusd: %v", p)
	if p["ProtectSystem"] != "strict" {
		t.Errorf("ProtectSystem=%s, want strict", p["ProtectSystem"])
	}
	for _, w := range strings.Fields(p["ReadWritePaths"]) {
		if w = strings.TrimLeft(w, "-+"); w == "/etc" || w == "/etc/nexus" || w == "/etc/" || w == "/etc/nexus/" {
			t.Errorf("ReadWritePaths %q contains %s", p["ReadWritePaths"], w)
		}
	}
	var want string
	for line := range strings.Lines(string(raRead(t, shippedUnit))) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "CapabilityBoundingSet="); ok {
			want = strings.ToLower(v)
		}
	}
	if want == "" || p["CapabilityBoundingSet"] != want {
		t.Errorf("CapabilityBoundingSet=%q, want %q, the unit file's value", p["CapabilityBoundingSet"], want)
	}
	cat := mustSh(t, detach.Systemctl, "cat", "--no-pager", nexusdUnit)
	var files []string
	// systemctl cat heads each file it prints with "# <path>"; the unit's
	// own comments are not paths of existing files.
	for line := range strings.Lines(cat) {
		if m := catHeaderRE.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil && exists(m[1]) {
			files = append(files, m[0])
		}
	}
	t.Logf("systemctl cat nexusd: %q", files)
	if len(files) != 1 || files[0] != "# "+installedUnit {
		t.Errorf("systemctl cat nexusd shows %q, want only %s and no drop-in", files, installedUnit)
	}
	requireExactUnit(t)
}

// configSetHostEnabled is TC-RA-94's steps on h: providers.host.enabled=true
// waited, from the default file. It returns the file before, the run and the
// operation.
func (h *raHost) configSetHostEnabled() ([]byte, smRun, apitypes.Operation) {
	h.t.Helper()
	orig := raRead(h.t, nexusdConfig)
	run, op := h.configSet("providers.host.enabled=true")
	if run.code != 0 {
		h.t.Fatalf("nexusdconfig update providers.host.enabled=true: exit %d, want 0", run.code)
	}
	requireSucceeded(h.t, op)
	return orig, run, op
}

// TC-RA-94 (WI 5.2; FR-7, FR-8, ASM-7, NFR-6): config set on real systemd
// applies the change, writes the backup and reloads, with no restart.
func TestTCRA94_ConfigSetOnRealSystemdAppliesTheChangeWritesThe(t *testing.T) {
	h := newRAHost(t)
	mainPID := unitPID(t)
	st0, _ := raStatus(t)
	seq0 := st0.Seq
	since := smWindow()

	orig, _, op := h.configSetHostEnabled()
	res := decodeResult[spec.ConfigUpdateResult](t, op)
	wantChanges := []spec.ConfigChange{{Key: "providers.host.enabled", Old: false, New: true}}
	if !res.Changed || !reflect.DeepEqual(res.Changes, wantChanges) || len(res.RestartRequired) != 0 ||
		res.BackupPath != raBackup || res.Restored {
		t.Errorf("result %s, want changed, changes %v, restartRequired [], backupPath %s", op.Result, wantChanges, raBackup)
	}

	cfg := raRead(t, nexusdConfig)
	if uid, gid, perm := fileStat(t, nexusdConfig); uid != 0 || gid != 0 || perm != 0o600 {
		t.Errorf("%s is %d:%d %04o, want root:root 0600", nexusdConfig, uid, gid, perm)
	}
	if v := raYAMLValue(t, cfg, "providers.host.enabled"); v != true {
		t.Errorf("%s decodes providers.host.enabled %v, want true", nexusdConfig, v)
	}
	if uid, gid, perm := fileStat(t, raBackup); uid != 0 || gid != 0 || perm != 0o600 {
		t.Errorf("%s is %d:%d %04o, want root:root 0600", raBackup, uid, gid, perm)
	}
	if !bytes.Equal(raRead(t, raBackup), orig) {
		t.Errorf("%s differs from the file before the change", raBackup)
	}
	st, ok := raStatus(t)
	if !ok || st.Seq != seq0+1 || !st.Applied || st.SHA256 != sha(t, nexusdConfig) {
		t.Errorf("reload-status.json %+v (present %t), want seq %d, applied, sha256 %s", st, ok, seq0+1,
			sha(t, nexusdConfig))
	}
	requirePC(t, op)
	if pid := unitPID(t); pid != mainPID {
		t.Errorf("MainPID %d, want %d unchanged", pid, mainPID)
	}
	j := unitJournal(t, nexusdUnit, since)
	for _, w := range []string{"Reloading nexusd.service", "Reloaded nexusd.service"} {
		if !strings.Contains(j, w) {
			t.Errorf("journalctl -u nexusd since %s lacks %q:\n%s", since, w, j)
		}
	}
	if r := h.cli("hostsummary", "get", "-o", "json"); r.code != 0 {
		t.Errorf("hostsummary get: exit %d, want 0", r.code)
	}
	r := h.cli("nexusdconfig", "get", "providers.host.enabled", "-o", "json")
	ks := jsonItems[spec.ConfigKey](t, r.stdout)
	if r.code != 0 || len(ks) != 1 || ks[0].Properties.Source != "file" || ks[0].Properties.Value != true {
		t.Errorf("nexusdconfig get providers.host.enabled: exit %d, %+v, want source file, value true", r.code, ks)
	}
	requireOpUnitRan(t, op.ID)
	waitOpUnitsIdle(t, raMaxRuntime)
	requireUnitGone(t, op.ID)
}

// TC-RA-95 (WI 5.2; NFR-3, NFR-6): comments in the installed default file
// survive a real config set: one changed line, equal comment maps.
func TestTCRA95_CommentsInTheInstalledDefaultFileSurviveAReal(t *testing.T) {
	h := newRAHost(t)
	h.configSetHostEnabled()
	bak, now := raRead(t, raBackup), raRead(t, nexusdConfig)
	bl, nl := strings.Split(string(bak), "\n"), strings.Split(string(now), "\n")
	if len(bl) != len(nl) {
		t.Fatalf("%s has %d lines, %s %d; want the same count", nexusdConfig, len(nl), raBackup, len(bl))
	}
	var diff []int
	for i := range bl {
		if bl[i] != nl[i] {
			diff = append(diff, i)
		}
	}
	if len(diff) != 1 {
		t.Fatalf("%d lines differ (%v), want exactly one", len(diff), diff)
	}
	i := diff[0]
	section := ""
	for j := i; j >= 0; j-- {
		k := strings.TrimSpace(bl[j])
		if strings.HasPrefix(bl[j], "  ") && !strings.HasPrefix(bl[j], "   ") && !strings.HasPrefix(k, "#") &&
			strings.HasSuffix(k, ":") {
			section = k
			break
		}
	}
	t.Logf("the one changed line, %d, under %q:\n- %s\n+ %s", i+1, section, bl[i], nl[i])
	if section != "host:" || !strings.HasPrefix(bl[i], "    enabled: false") || !strings.HasPrefix(nl[i], "    enabled: true") {
		t.Errorf("the changed line is %q -> %q under %q, want the enabled line under providers.host", bl[i], nl[i], section)
	}
	if got, want := raCommentMap(t, now), raCommentMap(t, bak); !reflect.DeepEqual(got, want) {
		t.Errorf("comment maps differ:\n got %v\nwant %v", got, want)
	}
	// Every comment of .bak is in the new file: the full comment lines
	// verbatim, and each trailing comment's text.
	var missing []string
	bakHash, nowHash := 0, 0
	for _, l := range nl {
		if strings.Contains(l, "#") {
			nowHash++
		}
	}
	for _, l := range bl {
		c := strings.Index(l, "#")
		if c < 0 {
			continue
		}
		bakHash++
		if !strings.Contains(string(now), l[c:]) {
			missing = append(missing, l[c:])
		}
	}
	t.Logf("lines with '#': %s %d, %s %d", raBackup, bakHash, nexusdConfig, nowHash)
	if bakHash != nowHash || len(missing) != 0 {
		t.Errorf("comment lines: .bak %d, new %d; missing from the new file: %q", bakHash, nowHash, missing)
	}
}

// TC-RA-96 (WI 5.2; NFR-3, FR-7): adding an absent key to the hand-commented
// file on the real host keeps every comment attached.
func TestTCRA96_AddingAnAbsentKeyToAHandCommentedFileOnTheReal(t *testing.T) {
	h := newRAHost(t)
	// The harness fixture, byte-equal to config.Edit's NFR-3 fixture
	// (TC-RA-E25), without providers.pihole and with the nexusd provider
	// enabled, as raHost's sed enables it in the default file.
	var ls []string
	for _, l := range strings.Split(string(raRead(t, raCommented)), "\n") {
		switch {
		case strings.HasPrefix(l, "  # Pi-hole stays off"), strings.HasPrefix(l, "  pihole:"):
			continue
		case l == "  host:":
			ls = append(ls, "  nexusd:", "    enabled: true")
		}
		ls = append(ls, l)
	}
	in := []byte(strings.Join(ls, "\n"))
	if raYAMLValue(t, in, "providers.pihole") != nil || raYAMLValue(t, in, "providers.nexusd.enabled") != true {
		t.Fatalf("the prepared fixture still has providers.pihole or lacks the nexusd provider:\n%s", in)
	}
	writeConfig(t, string(in))
	st0, _ := raStatus(t)
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	waitUntil(t, 15*time.Second, "the reload of the commented file", func() (bool, string) {
		st, ok := raStatus(t)
		return ok && st.Seq > st0.Seq && st.Applied && st.SHA256 == sha(t, nexusdConfig), fmt.Sprintf("%+v", st)
	})

	run, op := h.configSet("providers.pihole.enabled=false")
	if run.code != 0 {
		t.Fatalf("exit %d, want 0", run.code)
	}
	requireSucceeded(t, op)
	requirePC(t, op)
	bak, out := raRead(t, raBackup), raRead(t, nexusdConfig)
	if !bytes.Equal(bak, in) {
		t.Errorf("%s differs from the installed commented file", raBackup)
	}
	if v := raYAMLValue(t, out, "providers.pihole.enabled"); v != false {
		t.Errorf("providers.pihole.enabled = %v, want false", v)
	}
	before, after := raCommentMap(t, bak), raCommentMap(t, out)
	for k, c := range before {
		if after[k] != c {
			t.Errorf("comments of %q: %+v, want %+v", k, after[k], c)
		}
	}
	for k, c := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("new comments at %q: %+v", k, c)
		}
	}
	outLines := strings.Split(string(out), "\n")
	i := slices.Index(outLines, "  pihole:")
	if i < 0 || i+1 >= len(outLines) || outLines[i+1] != "    enabled: false" {
		t.Fatalf("no new block mapping \"  pihole:\\n    enabled: false\" in\n%s", out)
	}
	rest := slices.Delete(slices.Clone(outLines), i, i+2)
	if got, want := raNormalize(rest), raNormalize(strings.Split(string(bak), "\n")); got != want {
		t.Errorf("the file beyond the new lines differs from .bak after normalization:\n got:\n%s\nwant:\n%s", got, want)
	}
	t.Logf("new lines %d-%d: %q; %d comment paths unchanged", i+1, i+2, outLines[i:i+2], len(before))
}

// TC-RA-97 (WI 5.2; FR-7, FR-9): a config set that changes nothing touches
// nothing: no operation, unit, lane hold, file write or reload; while the
// lane is busy it is refused like any other request (A-14).
func TestTCRA97_AConfigSetThatChangesNothingTouchesNothingOnThe(t *testing.T) {
	h := newRAHost(t)
	if exists(raBackup) {
		t.Fatalf("%s exists on a fresh host", raBackup)
	}
	before := fileState(t)
	since := smWindow()
	ids0 := h.opIDs()
	mark := auditMark(t)

	r1 := h.cli("nexusdconfig", "update", "--yes", "providers.nexusd.enabled=true", "--verbose")
	if r1.code != 0 || !strings.Contains(r1.stdout, `"changed":false`) {
		t.Errorf("step 1: exit %d, stdout %q; want exit 0 with changed: false", r1.code, r1.stdout)
	}
	if strings.Contains(r1.stderr, "GET https://"+exactAddr+"/v1/operations/") || !strings.Contains(r1.stderr, "< 200 ") {
		t.Errorf("step 1: the CLI's trace shows a wait or no 200:\n%s", r1.stderr)
	}
	code, hdr, body := h.raDo(h.token, http.MethodPost, raUpdateAt, `{"changes":["providers.nexusd.enabled=true"]}`, nil)
	t.Logf("step 2: POST %s: %d %v %s", raUpdateAt, code, hdr, body)
	var res spec.ConfigUpdateResult
	if code != http.StatusOK || json.Unmarshal(body, &res) != nil || res.Changed || hdr.Get("Location") != "" {
		t.Errorf("step 2: %d, Location %q, body %s; want 200 with changed false and no Location", code,
			hdr.Get("Location"), body)
	}
	requireFileState(t, "before step 3", before)
	if exists(raBackup) {
		t.Errorf("%s exists after a no-op set", raBackup)
	}
	if u := opUnitsInJournal(t, since); len(u) != 0 {
		t.Errorf("the journal since %s names %q, want no nexus-op-* unit", since, u)
	}
	if u := smOpUnits(); len(u) != 0 {
		t.Errorf("systemctl list-units 'nexus-op-*' --all: %v, want none", u)
	}
	if ids := h.opIDs(); !slices.Equal(ids, ids0) {
		t.Errorf("GET /v1/operations: %q, want %q unchanged", ids, ids0)
	}
	pre := auditSince(t, mark)

	r3 := h.cli("nexusd", "reload", "--yes", "-o", "json", "--verbose")
	if r3.code != 0 || !strings.Contains(r3.stderr, "< 202 ") || len(r3.ops()) != 1 ||
		r3.ops()[0].Status != apitypes.StatusSucceeded {
		t.Errorf("step 3: nexus nexusd reload exit %d, want 202 and Succeeded: the no-op held the lane", r3.code)
	}

	g := h.held()
	id := g.gate("providers.host.enabled=true")
	mark5 := auditMark(t)
	r5 := h.cli("nexusdconfig", "update", "--yes", "providers.nexusd.enabled=true")
	p := requireRefused(t, r5, problem.Conflict, http.StatusConflict)
	if fmt.Sprint(p["blockingOperationId"]) != id && !strings.Contains(fmt.Sprint(p["detail"]), id) {
		t.Errorf("step 5: the conflict %v does not name %s", p, id)
	}
	a5 := auditSince(t, mark5)
	g.open()
	op := h.awaitOp(id, raOpBound)
	requireSucceeded(t, op)
	requirePC(t, op)

	// Audit: steps 1 and 2 are requested then completed, never queued or
	// running; step 5 is refused conflict with no completed record.
	var noops []string
	for _, r := range pre {
		switch r.Outcome {
		case "queued", "running":
			t.Errorf("audit before step 3 has a %s record: %+v", r.Outcome, r)
		case "requested":
			if r.Action == spec.UpdateConfig.Op().ID {
				noops = append(noops, r.ID)
			}
		}
	}
	for _, rid := range noops {
		done := slices.ContainsFunc(pre, func(r raAudit) bool { return r.Outcome == "completed" && r.RequestID == rid })
		if !done {
			t.Errorf("audit: requested %s has no completed record", rid)
		}
	}
	if len(noops) != 2 {
		t.Errorf("audit: %d requested update-config records for steps 1 and 2, want 2: %+v", len(noops), pre)
	}
	refused, completed := 0, 0
	for _, r := range a5 {
		if r.Action != spec.UpdateConfig.Op().ID {
			continue
		}
		switch {
		case r.Outcome == "refused" && r.Problem == string(problem.Conflict):
			refused++
		case r.Outcome == "completed":
			completed++
		}
	}
	if refused != 1 || completed != 0 {
		t.Errorf("audit for step 5: %d refused conflict, %d completed; want 1 and 0: %+v", refused, completed, a5)
	}
}

// TC-RA-98 (WI 5.2; FR-8, ASM-4, ASM-3): a restart-only key is written and
// reported, never applied by a restart. tls.mode off is valid on the default
// loopback listen; audit.* and auth.* are on the deny-list (SEC-2).
func TestTCRA98_ARestartOnlyKeyIsWrittenAndReportedNeverApplied(t *testing.T) {
	h := newRAHost(t)
	pid0 := unitPID(t)
	run, op := h.configSet("tls.mode=off", "limits.outputCapBytes=20971520")
	requireSucceeded(t, op)
	res := decodeResult[spec.ConfigUpdateResult](t, op)
	if !slices.Equal(res.RestartRequired, []string{"limits", "tls"}) {
		t.Errorf("restartRequired %q, want [limits tls]", res.RestartRequired)
	}
	for _, w := range []string{"tls.mode takes effect when nexusd restarts", "run nexus nexusd restart to apply"} {
		if !strings.Contains(run.stderr, w) {
			t.Errorf("stderr lacks %q:\n%s", w, run.stderr)
		}
	}
	if pid := unitPID(t); pid != pid0 {
		t.Errorf("MainPID %d, want %d: config set restarted nexusd", pid, pid0)
	}
	if n := showUnit(t, nexusdUnit, "NRestarts")["NRestarts"]; n != h.nrestarts0 {
		t.Errorf("NRestarts %s, want %s", n, h.nrestarts0)
	}
	cfg := raRead(t, nexusdConfig)
	if a, b := raYAMLValue(t, cfg, "tls.mode"), raYAMLValue(t, cfg, "limits.outputCapBytes"); a != "off" || b != 20971520 {
		t.Errorf("file holds tls.mode %v and limits.outputCapBytes %v, want off and 20971520", a, b)
	}
	st, _ := raStatus(t)
	if !slices.Equal(st.RestartRequired, []string{"limits", "tls"}) {
		t.Errorf("reload-status.json restartRequired %q, want [limits tls]", st.RestartRequired)
	}
	requirePC(t, op)
}

// TC-RA-99 (WI 5.2; FR-9): the nexusd lane refuses a reload, a restart, a
// second config set and a no-change set while a config set runs.
func TestTCRA99_TheNexusdLaneRefusesAReloadWhileAConfigSetRunsOn(t *testing.T) {
	h := newRAHost(t)
	pid0 := unitPID(t)
	g := h.held()
	id := g.gate("providers.host.enabled=true")
	ids0 := h.opIDs()
	for _, args := range [][]string{
		{"nexusd", "reload", "--yes"},
		{"nexusd", "restart", "--yes"},
		{"nexusdconfig", "update", "--yes", "limits.outputCapBytes=20971520"},
		{"nexusdconfig", "update", "--yes", "providers.nexusd.enabled=true"},
	} {
		p := requireRefused(t, h.cli(args...), problem.Conflict, http.StatusConflict)
		if fmt.Sprint(p["blockingOperationId"]) != id && !strings.Contains(fmt.Sprint(p["detail"]), id) {
			t.Errorf("nexus %s: the conflict %v does not name %s", strings.Join(args, " "), p, id)
		}
	}
	if d := time.Since(g.reached); d > 20*time.Second {
		t.Errorf("step 2 ended %s after reached, want within 20 s", d)
	}
	if exists(raHoldOpen) {
		t.Fatalf("%s exists: step 2 did not run while %s was held", raHoldOpen, id)
	}
	if ids := h.opIDs(); !slices.Equal(ids, ids0) {
		t.Errorf("GET /v1/operations: %q, want %q: a refused call created an operation", ids, ids0)
	}
	if pid := unitPID(t); pid != pid0 {
		t.Errorf("MainPID %d, want %d", pid, pid0)
	}
	if n := showUnit(t, nexusdUnit, "NRestarts")["NRestarts"]; n != h.nrestarts0 {
		t.Errorf("NRestarts %s, want %s", n, h.nrestarts0)
	}
	g.open()
	op := h.awaitOp(id, raOpBound)
	requireSucceeded(t, op)
	requirePC(t, op)
	g.release()
	r := h.cli("nexusd", "reload", "--yes", "-o", "json")
	if r.code != 0 || len(r.ops()) != 1 || r.ops()[0].Status != apitypes.StatusSucceeded {
		t.Errorf("step 5: nexus nexusd reload exit %d, want a Succeeded reload", r.code)
	}
}

// raSecret is one created token: the string, its secret part and the
// sha256 hex tokens.json stores.
type raSecret struct{ name, token, secret, hash string }

func (s raSecret) needles() []string { return []string{s.token, s.secret, s.hash} }

// tokenHashes reads tokens.json's sha256 per token name.
func tokenHashes(t *testing.T) map[string]string {
	t.Helper()
	var f struct {
		Tokens []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raRead(t, raTokens), &f); err != nil {
		t.Fatalf("%s: %v", raTokens, err)
	}
	m := map[string]string{}
	for _, tk := range f.Tokens {
		m[tk.Name] = tk.SHA256
	}
	return m
}

// TC-RA-100 (WI 5.2; NFR-1, FR-1, FR-13, NFR-6): token secrets never reach
// state.db, ops/, the audit log, the journal or the operation and audit
// endpoints on the real host.
func TestTCRA100_TokenSecretsNeverReachStateDbTheAuditLogThe(t *testing.T) {
	h := newRAHost(t)
	// GET /v1/audit needs audit:read, which raHost's token lacks; this token
	// reads it (test-cases.md names it /v1/system/audit; the route is
	// /v1/audit, system.audit.list).
	auditor := createToken(t, "auditor", "audit:read")
	since := smWindow()
	var stderrs []string

	code, hdr, body1 := h.raDo(h.token, http.MethodPost, raTokensAt+"/t1rest/add", `{}`, nil)
	var add1 spec.TokenAddResult
	if code != http.StatusOK || json.Unmarshal(body1, &add1) != nil || add1.Token == "" {
		t.Fatalf("step 1: %d %s, want 200 with a token", code, body1)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("step 1: Cache-Control %q, want no-store", cc)
	}
	r2 := h.cli("nexusdtoken", "add", "t1cli", "--yes", "-o", "json")
	stderrs = append(stderrs, r2.stderr)
	var add2 spec.TokenAddResult
	if adds := jsonItems[spec.TokenAddResult](t, r2.stdout); r2.code == 0 && len(adds) == 1 {
		add2 = adds[0]
	}
	if add2.Token == "" {
		t.Fatalf("step 2: exit %d, stdout %q, want a token", r2.code, r2.stdout)
	}
	n0 := len(tokenHashes(t))
	code, _, body3 := h.raDo(h.token, http.MethodPost, raTokensAt+"/t1rest/add", `{}`,
		map[string]string{"Idempotency-Key": "tc-ra-100"})
	var p3 map[string]any
	if json.Unmarshal(body3, &p3) != nil || code != http.StatusBadRequest || smSlug(p3) != string(problem.ValidationFailed) {
		t.Errorf("step 3: %d %s, want 400 validation-failed", code, body3)
	}
	if n := len(tokenHashes(t)); n != n0 {
		t.Errorf("step 3: %d tokens, want %d unchanged", n, n0)
	}
	hashes := tokenHashes(t)
	var secrets []raSecret
	for _, a := range []spec.TokenAddResult{add1, add2} {
		parts := strings.SplitN(a.Token, "_", 3)
		if len(parts) != 3 || hashes[a.Name] == "" {
			t.Fatalf("token %s: %q is not nxs_<id>_<secret>, or tokens.json has no hash for it", a.Name, a.Token)
		}
		secrets = append(secrets, raSecret{name: a.Name, token: a.Token, secret: parts[2], hash: hashes[a.Name]})
	}
	// Non-vacuity: the answers carried their secret, and tokens.json both
	// hashes.
	if !strings.Contains(string(body1), secrets[0].token) || !strings.Contains(r2.stdout, secrets[1].token) {
		t.Error("non-vacuity: the step 1 body or step 2 stdout lacks its token")
	}
	tj := string(raRead(t, raTokens))
	for _, s := range secrets {
		if !strings.Contains(tj, s.hash) {
			t.Errorf("non-vacuity: tokens.json lacks %s's hash", s.name)
		}
	}

	if code, _, b := h.raDo(h.token, http.MethodPost, raTokensAt+"/t1rest/remove", `{}`, nil); code != http.StatusOK {
		t.Errorf("step 4: remove t1rest: %d %s", code, b)
	}
	r4 := h.cli("nexusdtoken", "remove", "t1cli", "--yes", "-o", "json")
	stderrs = append(stderrs, r4.stderr)
	if r4.code != 0 {
		t.Errorf("step 4: remove t1cli: exit %d", r4.code)
	}

	sinks := map[string][]byte{}
	files, err := filepath.Glob(raStateDB + "*")
	if err != nil {
		t.Fatal(err)
	}
	audits, err := filepath.Glob(smAudit + "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(files, audits...) {
		sinks[f] = raRead(t, f)
	}
	if len(files) == 0 || len(audits) == 0 {
		t.Fatalf("non-vacuity: state.db* %q, audit.jsonl* %q", files, audits)
	}
	if err := filepath.WalkDir(smOpsDir, func(p string, d fs.DirEntry, err error) error {
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
	sinks["journalctl -u nexusd"] = []byte(mustSh(t, "journalctl", "-u", nexusdUnit, "--since", since, "--no-pager", "-o", "json"))
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
	sinks["step 3 problem body"] = body3
	for _, id := range h.opIDs() {
		r := h.cli("operation", "get", id, "-o", "json")
		sinks["nexus operation get "+id] = []byte(r.stdout + r.stderr)
		if ops := r.ops(); len(ops) == 1 && strings.HasPrefix(ops[0].Action, spec.Name+".tokens.") {
			t.Errorf("operation %s is a token op %s: token ops never create operations", id, ops[0].Action)
		}
	}
	sinks["CLI stderr"] = []byte(strings.Join(stderrs, "\n"))
	if !bytes.Contains(auditBodies, []byte("nexusd.tokens.add")) {
		t.Error("non-vacuity: GET /v1/audit has no nexusd.tokens.add record")
	}
	for name, b := range sinks {
		for _, s := range secrets {
			for _, n := range s.needles() {
				if c := bytes.Count(b, []byte(n)); c != 0 {
					t.Errorf("%s holds %s's secret material %d times", name, s.name, c)
				}
			}
		}
	}
	t.Logf("swept %d sinks for %d needles each of %d tokens: 0 occurrences expected", len(sinks), 3, len(secrets))
}

// TC-RA-101 (WI 5.2; FR-4, FR-1, FR-3, NFR-6): a remote add and a local sudo
// create of one name race on the real host with one winner, and add and
// revoke apply on the next request.
func TestTCRA101_RemoteAddAndLocalSudoCreateOfOneNameRaceOnThe(t *testing.T) {
	h := newRAHost(t)
	since := smWindow()
	type outcome struct {
		code       int
		body       []byte
		exit       int
		stderr     string
		err        error
		begin, end time.Time
	}
	// Launched together, the REST add always reaches the daemon first (the
	// local create has to exec a process), so the two never meet. Measure what
	// a local create takes and hold the REST add back by a growing share of
	// it, 0 to 1.2 times: the early rounds are won by REST, the late ones by
	// the local create, and the ones between meet at the token file's lock.
	var calib []time.Duration
	for i := range 3 {
		t0 := time.Now()
		if out, err := exec.Command(nexusdBin, "token", "create", "--name", fmt.Sprintf("calib%d", i)).CombinedOutput(); err != nil {
			t.Fatalf("calibration create %d: %v: %s", i, err, out)
		}
		calib = append(calib, time.Since(t0))
	}
	slices.Sort(calib)
	local1 := calib[len(calib)/2]
	const rounds = 10
	var restWins, localWins, overlapped int
	for i := range rounds {
		name := fmt.Sprintf("race%d", i)
		delay := local1 * 12 / 10 * time.Duration(i) / (rounds - 1)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var rest, local outcome
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(delay)
			rest.begin = time.Now()
			defer func() { rest.end = time.Now() }()
			req, err := http.NewRequest(http.MethodPost, "https://"+exactAddr+raTokensAt+"/"+name+"/add", strings.NewReader(`{}`))
			if err != nil {
				rest.err = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+h.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.http.Do(req)
			if err != nil {
				rest.err = err
				return
			}
			defer resp.Body.Close()
			rest.code = resp.StatusCode
			rest.body, rest.err = io.ReadAll(resp.Body)
		}()
		go func() {
			defer wg.Done()
			<-start
			local.begin = time.Now()
			cmd := exec.Command(nexusdBin, "token", "create", "--name", name)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			local.end = time.Now()
			local.stderr = stderr.String()
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				local.exit = ee.ExitCode()
			case err != nil:
				local.err = err
			}
		}()
		close(start)
		wg.Wait()
		if rest.err != nil || local.err != nil {
			t.Fatalf("round %d: %v %v", i, rest.err, local.err)
		}
		t.Logf("round %d: REST held back %s, %s to %s: %d %s; nexusd token create %s to %s: exit %d %s", i,
			delay.Round(time.Millisecond), rest.begin.Format("05.000"), rest.end.Format("05.000"), rest.code, rest.body,
			local.begin.Format("05.000"), local.end.Format("05.000"), local.exit, strings.TrimSpace(local.stderr))
		if rest.begin.Before(local.end) && local.begin.Before(rest.end) {
			overlapped++
		}
		wins := 0
		if rest.code == http.StatusOK {
			wins++
			restWins++
		} else if rest.code != http.StatusConflict {
			t.Errorf("round %d: REST answered %d, want 200 or 409", i, rest.code)
		}
		if local.exit == 0 {
			wins++
			localWins++
		} else if !strings.Contains(local.stderr, "already exists") {
			t.Errorf("round %d: the local create failed other than for the duplicate name: %s", i, local.stderr)
		}
		if wins != 1 {
			t.Errorf("round %d: %d winners, want exactly 1", i, wins)
		}
	}
	t.Logf("a local create takes %s: REST won %d rounds, the local create %d, the two overlapped in %d", local1, restWins,
		localWins, overlapped)
	if restWins == 0 || localWins == 0 || overlapped == 0 {
		t.Errorf("REST won %d rounds, the local create %d, and their intervals overlapped in %d: want each side "+
			"to win at least once and the two to overlap, or the rounds never raced", restWins, localWins, overlapped)
	}
	var f struct {
		Tokens []struct {
			Name string `json:"name"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raRead(t, raTokens), &f); err != nil {
		t.Fatalf("%s is not valid JSON: %v", raTokens, err)
	}
	if uid, gid, perm := fileStat(t, raTokens); uid != 0 || gid != 0 || perm != 0o600 {
		t.Errorf("%s is %d:%d %04o, want root:root 0600", raTokens, uid, gid, perm)
	}
	for i := range rounds {
		name := fmt.Sprintf("race%d", i)
		if n := slices.IndexFunc(f.Tokens, func(x struct {
			Name string `json:"name"`
		}) bool {
			return x.Name == name
		}); n < 0 {
			t.Errorf("%s has no %s record", raTokens, name)
		}
		c := 0
		for _, x := range f.Tokens {
			if x.Name == name {
				c++
			}
		}
		if c != 1 {
			t.Errorf("%s has %d %s records, want 1", raTokens, c, name)
		}
	}
	for _, e := range raJournal(t, since, nexusdUnit) {
		if strings.Contains(e.msg, "EACCES") || strings.Contains(e.msg, "Permission denied") ||
			strings.Contains(e.msg, "tokens.json.lock") || strings.Contains(e.msg, "Read-only file system") {
			t.Errorf("journal: a sandbox denial: %s", e.msg)
		}
	}

	code, _, b := h.raDo(h.token, http.MethodPost, raTokensAt+"/nx/add", `{"scopes":["`+spec.ReadScope+`"]}`, nil)
	var nx spec.TokenAddResult
	if code != http.StatusOK || json.Unmarshal(b, &nx) != nil || nx.Token == "" {
		t.Fatalf("step 2: add nx: %d %s", code, b)
	}
	if code, _, b := h.raDo(nx.Token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusOK {
		t.Errorf("step 2: GET %s with nx: %d %s, want 200", smDaemonsAt, code, b)
	}
	if code, _, b := h.raDo(h.token, http.MethodPost, raTokensAt+"/nx/remove", `{}`, nil); code != http.StatusOK {
		t.Fatalf("step 3: remove nx: %d %s", code, b)
	}
	if code, _, b := h.raDo(nx.Token, http.MethodGet, smDaemonsAt, "", nil); code != http.StatusUnauthorized {
		t.Errorf("step 3: GET %s with the removed nx: %d %s, want 401", smDaemonsAt, code, b)
	}
	r := h.cli("nexusdtoken", "get", "-o", "json")
	var selfs []string
	for _, tk := range jsonItems[spec.Token](t, r.stdout) {
		if tk.Properties.Self {
			selfs = append(selfs, tk.Name)
		}
	}
	if r.code != 0 || !slices.Equal(selfs, []string{"ra"}) {
		t.Errorf("nexusdtoken get: exit %d, self true for %q, want only ra", r.code, selfs)
	}
}

// systemctlReloadInactive is what `systemctl reload nexusd.service` printed
// on this harness's real systemd 259 for a stopped unit (captured by
// TestTCRAE33's run, 2026-11-17). internal/providers/nexusd's
// reload_classify_test.go reads this same file by a relative path and feeds
// it to the helper's classification; this test fails when a systemd says
// something else, so the golden cannot drift from the real wording.
// systemd's own source: src/shared/bus-wait-for-jobs.c, check_wait_response,
// `%s is not active, cannot reload.` for a job result of "invalid".
//
//go:embed testdata/systemctl-reload-inactive.txt
var systemctlReloadInactive string

// TC-RA-E33 (WI 4.2; FR-8; code review CR-10, CR-17): on real systemd,
// `systemctl reload` of a unit that is not active fails with the text the
// helper's retry classification matches (TC-RA-72 is the T0 retry test). The
// unit tests feed classifyReloadError the capture of this run
// (test/integration/testdata/systemctl-reload-inactive.txt), so this is where
// the wording is pinned to a real systemd, not to a string the fake composes.
func TestTCRAE33_OnRealSystemdReloadOfAnInactiveUnitSaysNotActive(t *testing.T) {
	h := newRAHost(t)
	mustSh(t, "systemctl", "stop", nexusdUnit)
	t.Cleanup(func() { startExact(t); h.waitHealthz() })
	if st := showUnit(t, nexusdUnit, "ActiveState")["ActiveState"]; st != "inactive" {
		t.Fatalf("nexusd.service ActiveState %q after stop, want inactive", st)
	}
	out, err := sh(30*time.Second, "systemctl", "reload", nexusdUnit)
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("systemctl reload of a stopped unit: %v, want a non-zero exit; output %q", err, out)
	}
	t.Logf("CAPTURE systemctl reload %s (stopped): exit %d, output %q", nexusdUnit, ee.ExitCode(), out)
	if out != systemctlReloadInactive {
		t.Errorf("output %q, want the golden %q (test/integration/testdata/systemctl-reload-inactive.txt): the "+
			"helper's not-active classification would no longer match", out, systemctlReloadInactive)
	}
}

// waitHealthz waits for GET /healthz to answer 200 over the pinned
// connection.
func (h *raHost) waitHealthz() {
	h.t.Helper()
	waitUntil(h.t, time.Minute, "GET /healthz 200", func() (bool, string) {
		resp, err := h.http.Get("https://" + exactAddr + "/healthz")
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body) // Only the status matters
		return resp.StatusCode == http.StatusOK, resp.Status
	})
}

// TC-RA-102 (WI 5.2; NFR-2): the latencies on the real host, with 100
// stored tokens.
func TestTCRA102_NFR2LatenciesOnTheRealHost(t *testing.T) {
	h := newRAHost(t)
	for i := range 100 {
		createToken(t, fmt.Sprintf("bulk%03d", i))
	}
	t.Logf("%d tokens stored", len(tokenHashes(t)))
	timed := func(token, method, path, body string, want int) time.Duration {
		start := time.Now()
		code, _, b := h.raDo(token, method, path, body, nil)
		d := time.Since(start)
		if code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, code, b, want)
		}
		return d
	}
	var lists, gets, adds, removes []time.Duration
	for range 50 {
		lists = append(lists, timed(h.token, http.MethodGet, raTokensAt, "", http.StatusOK))
		gets = append(gets, timed(h.token, http.MethodGet, raConfigAt+"providers.host.enabled", "", http.StatusOK))
	}
	for batch := range 10 {
		if batch > 0 {
			// A restart empties the in-memory mutation window (A-1). The
			// whole run starts nexusd.service many times within systemd's
			// default start limit (5 per 10 s); reset-failed resets that
			// counter and changes nothing else of the unit.
			mustSh(t, detach.Systemctl, "reset-failed", nexusdUnit)
			mustSh(t, detach.Systemctl, "restart", nexusdUnit)
			h.waitHealthz()
		}
		for i := range 5 {
			name := fmt.Sprintf("nfr%d-%d", batch, i)
			adds = append(adds, timed(h.token, http.MethodPost, raTokensAt+"/"+name+"/add", `{}`, http.StatusOK))
			removes = append(removes, timed(h.token, http.MethodPost, raTokensAt+"/"+name+"/remove", `{}`, http.StatusOK))
		}
	}
	for _, m := range []struct {
		what string
		ds   []time.Duration
	}{{"token list p95 (50)", lists}, {"config get p95 (50)", gets}, {"token add p95 (50)", adds},
		{"token remove p95 (50)", removes}} {
		nfrLine(t, m.what, p95(m.ds), raP95Budget)
	}

	// Step 3: a config set with a passing reload, the 202 to ended.
	since := smWindow()
	code, _, b := h.raDo(h.token, http.MethodPost, raUpdateAt, `{"changes":["providers.host.enabled=true"]}`, nil)
	t202 := time.Now()
	var acc apitypes.Operation
	if code != http.StatusAccepted || json.Unmarshal(b, &acc) != nil || acc.ID == "" {
		t.Fatalf("step 3: %d %s, want 202 with an operation", code, b)
	}
	op := h.awaitOp(acc.ID, raOpBound)
	requireSucceeded(t, op)
	requirePC(t, op)
	st, _ := raStatus(t)
	var reloading time.Time
	for _, e := range raJournal(t, since, nexusdUnit) {
		if strings.HasPrefix(e.msg, "Reloading nexusd.service") {
			reloading = e.at
		}
	}
	if reloading.IsZero() || st.At.Before(reloading) {
		t.Fatalf("step 3: no Reloading line before the status at %s (Reloading at %s)", st.At, reloading)
	}
	reloadTime := st.At.Sub(reloading)
	t.Logf("NFR NFR-2 config set reload's own time: %s (journal Reloading %s to reload-status.json at %s)",
		reloadTime.Round(time.Millisecond), reloading.Format(time.RFC3339Nano), st.At.Format(time.RFC3339Nano))
	nfrLine(t, "config set with a passing reload, 202 to ended", op.Ended.Sub(t202), raSetBudget+reloadTime)

	// Steps 4 and 5: the restoring paths, from tOpen.
	r4 := h.heldRefusedRestore("limits.outputCapBytes=31457280")
	requireRestored(t, r4)
	nfrLine(t, "config set refused at load and restored, tOpen to ended", r4.took, raRestoreBudget)
	r5 := h.heldUnconfirmedRestore("limits.outputCapBytes=33554432")
	requireRestored(t, r5)
	nfrLine(t, "config set unconfirmed and restored, tOpen to ended", r5.took, raRestoreBudget)
}

// TC-RA-E26 (WI 5.2; FR-8, NFR-6, NFR-2): on real systemd an unconfirmed
// reload is restored and confirmed, within 30 s of the gate's opening.
func TestTCRAE26_OnRealSystemdAnUnconfirmedReloadIsRestoredAnd(t *testing.T) {
	h := newRAHost(t)
	r := h.heldUnconfirmedRestore("providers.host.enabled=true")
	requireRestored(t, r)
	h.waitHealthz()
	nfrLine(t, "config set unconfirmed and restored (TC-RA-E26), tOpen to ended", r.took, raRestoreBudget)
}

// TC-RA-ERR32 (WI 5.2; FR-7, NFR-6): a validation refusal starts no unit
// and touches nothing.
func TestTCRAERR32_OnRealSystemdAValidationRefusalStartsNoUnitAnd(t *testing.T) {
	h := newRAHost(t)
	before := fileState(t)
	units0 := smOpUnits()
	ids0 := h.opIDs()
	since := smWindow()
	for _, c := range []struct{ change, path string }{
		{"limits.outputCapBytes=0", "limits.outputCapBytes"},
		{"providers.host.enabeld=true", "providers.host.enabeld"},
		{"providers.host.enabled=yes", "providers.host.enabled"},
	} {
		r := h.cli("nexusdconfig", "update", "--yes", c.change)
		p := requireRefused(t, r, problem.ConfigInvalid, http.StatusUnprocessableEntity)
		errs := problemErrors(p)
		if len(errs) != 1 || errs[0].Path != c.path || errs[0].Message == "" || !strings.Contains(r.stderr, c.path) {
			t.Errorf("%s: errors %+v, want one located at %s, printed", c.change, errs, c.path)
		}
	}
	requireFileState(t, "after the refusals", before)
	if u := opUnitsInJournal(t, since); len(u) != 0 {
		t.Errorf("the journal since %s names %q, want no nexus-op-* unit", since, u)
	}
	if u := smOpUnits(); !reflect.DeepEqual(u, units0) {
		t.Errorf("nexus-op-* units %v, want %v", u, units0)
	}
	if ids := h.opIDs(); !slices.Equal(ids, ids0) {
		t.Errorf("GET /v1/operations: %q, want %q", ids, ids0)
	}
}

// raDenyValue is TC-RA-ERR33 step 1's value for a deny-list entry.
func raDenyValue(d spec.ConfigDeny) string {
	switch {
	case d.Value != "":
		return d.Value
	case strings.HasSuffix(d.Key, ".trustedKeys"), d.Subtree:
		return "x"
	case strings.HasSuffix(d.Key, ".includeDir"):
		return "/etc/profile.d"
	case strings.HasSuffix(d.Key, ".includeSuffix"), strings.HasSuffix(d.Key, ".disabledSuffix"):
		return ".sh"
	case strings.HasSuffix(d.Key, ".service"):
		return "ssh"
	case d.Key == "audit.retentionDays":
		return "1"
	case d.Key == "auth.failureLimitPerMinute":
		return "1000000"
	}
	return "/etc/nexus/x.conf"
}

// sshState is `systemctl is-active` of ssh and sshd.
func sshState() string {
	out, _ := sh(30*time.Second, detach.Systemctl, "is-active", "ssh.service", "sshd.service")
	return strings.Join(strings.Fields(out), " ")
}

// TC-RA-ERR33 (WI 5.2; FR-12, NFR-6, NFR-5, OOS-3): each deny-list entry is
// refused with its reason, and nothing it names is touched.
func TestTCRAERR33_OnRealSystemdEachDenyListEntryIsRefusedWithIts(t *testing.T) {
	h := newRAHost(t)
	if len(spec.ConfigDenyList) != 14 {
		t.Fatalf("spec.ConfigDenyList has %d entries, want fourteen", len(spec.ConfigDenyList))
	}
	before := fileState(t)
	profile0 := mustSh(t, "ls", "-la", "/etc/profile.d")
	ssh0 := sshState()
	since := smWindow()
	mark := auditMark(t)
	calls := 0
	for _, d := range spec.ConfigDenyList {
		change := d.Key + "=" + raDenyValue(d)
		r := h.cli("nexusdconfig", "update", "--yes", change)
		calls++
		p := requireRefused(t, r, problem.ConfigInvalid, http.StatusUnprocessableEntity)
		errs := problemErrors(p)
		if len(errs) != 1 || errs[0].Path != d.Key || !strings.Contains(errs[0].Message, "cannot be set remotely") ||
			!strings.Contains(errs[0].Message, d.Reason) {
			t.Errorf("%s: errors %+v, want one at %s with \"cannot be set remotely\" and %q", change, errs, d.Key, d.Reason)
		}
	}
	r := h.cli("nexusdconfig", "update", "--yes", "providers.nginx.includeDir=/etc/profile.d", "providers.nginx.includeSuffix=.sh")
	calls++
	errs := problemErrors(requireRefused(t, r, problem.ConfigInvalid, http.StatusUnprocessableEntity))
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Path)
		if !strings.Contains(e.Message, "cannot be set remotely") {
			t.Errorf("step 2: %+v lacks \"cannot be set remotely\"", e)
		}
	}
	if !slices.Equal(paths, []string{"providers.nginx.includeDir", "providers.nginx.includeSuffix"}) {
		t.Errorf("step 2 names %q, want both keys", paths)
	}
	r = h.cli("nexusdconfig", "update", "--yes", "listen=127.0.0.1:9")
	calls++
	errs = problemErrors(requireRefused(t, r, problem.ConfigInvalid, http.StatusUnprocessableEntity))
	if len(errs) != 1 || errs[0].Path != "listen" || !strings.Contains(errs[0].Message, "is a list") {
		t.Errorf("step 3: errors %+v, want one at listen with the list message", errs)
	}
	if strings.Contains(r.stderr, "locked out") || strings.Contains(r.stderr, "takes effect when nexusd restarts") {
		t.Errorf("step 3: stderr has a lock-out warning:\n%s", r.stderr)
	}
	if p := mustSh(t, "ls", "-la", "/etc/profile.d"); p != profile0 {
		t.Errorf("/etc/profile.d changed:\n%s\nwas:\n%s", p, profile0)
	}
	if s := sshState(); s != ssh0 {
		t.Errorf("ssh is %q, was %q", s, ssh0)
	}
	if l := listeners(t, unitPID(t)); !slices.Equal(l, []string{exactAddr}) {
		t.Errorf("nexusd listens on %q, want only %s", l, exactAddr)
	}
	requireFileState(t, "after the refusals", before)
	if u := opUnitsInJournal(t, since); len(u) != 0 {
		t.Errorf("the journal since %s names %q, want no nexus-op-* unit", since, u)
	}
	refused := 0
	for _, a := range auditSince(t, mark) {
		if a.Action == spec.UpdateConfig.Op().ID && a.Outcome == "refused" && a.Problem == string(problem.ConfigInvalid) {
			refused++
		}
	}
	if refused != calls {
		t.Errorf("audit: %d refused config-invalid records, want one per call (%d)", refused, calls)
	}
}

// TC-RA-ERR34 (WI 5.2; FR-8, NFR-6, ASM-7): a reload refused at load
// restores the backup and confirms the restore. An apply-stage refusal with
// valid content is T0-only (F-RELOAD refuseApply, TC-RA-ERR21): admission
// already runs every provider's Configure, so a real daemon cannot refuse
// valid content at apply.
func TestTCRAERR34_OnRealSystemdAReloadRefusedAtLoadRestoresThe(t *testing.T) {
	h := newRAHost(t)
	orig := raRead(t, nexusdConfig)
	st0, _ := raStatus(t)
	r := h.heldRefusedRestore("providers.host.enabled=true")
	requireRestored(t, r)
	if d := r.op.Error.Detail; !strings.Contains(d, "stage load") {
		t.Errorf("detail %q, want the refusal at stage load", d)
	}
	if !bytes.Equal(raRead(t, raBackup), orig) {
		t.Errorf("%s differs from the original", raBackup)
	}
	// The status before the last: seq0+1 refused at load with no digest,
	// in the journal; the last, seq0+2, the restore applied.
	if st, _ := raStatus(t); st.Seq != st0.Seq+2 {
		t.Errorf("reload-status.json seq %d, want %d: one refused reload, then the restore's", st.Seq, st0.Seq+2)
	}
	refusedLine := false
	for _, e := range raJournal(t, "@"+strconv.FormatInt(r.tOpen.Unix()-1, 10), nexusdUnit) {
		if strings.Contains(e.msg, "configuration reload failed") {
			refusedLine = true
			t.Logf("journal: %s", e.msg)
		}
	}
	if !refusedLine {
		t.Error("the journal shows no refused reload (stage load) after tOpen")
	}
	if g := h.cli("nexusd", "get", "-o", "json"); g.code != 0 {
		t.Errorf("nexusd get: exit %d, want 0", g.code)
	}
	if hs := h.cli("hostsummary", "get", "-o", "json"); hs.code == 0 {
		t.Errorf("hostsummary get succeeded: providers.host.enabled is in force, want it still false")
	} else {
		t.Logf("hostsummary get refused as disabled: %v", hs.problem())
	}
	t.Logf("tOpen to ended %s", r.took.Round(time.Millisecond))
}

// raOther is the installed file with audit.retentionDays: 45, written to a
// temporary path root 0600.
func raOther(t *testing.T) (string, string) {
	t.Helper()
	orig := string(raRead(t, nexusdConfig))
	other := strings.Replace(orig, "\n  retentionDays: 90\n", "\n  retentionDays: 45\n", 1)
	if other == orig {
		t.Fatal("the installed file has no audit retentionDays: 90 line")
	}
	p := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(p, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, sha256Sum([]byte(other))
}

// TC-RA-ERR35 (WI 5.2; FR-8, FR-7, NFR-6): a file replaced before the reload
// ends config-conflict, kept and not restored.
func TestTCRAERR35_OnRealSystemdAFileReplacedBeforeTheReloadEnds(t *testing.T) {
	h := newRAHost(t)
	other, otherSHA := raOther(t)
	orig := sha(t, nexusdConfig)
	g := h.held()
	id := g.gate("providers.host.enabled=true")
	g.stop()
	tOpen := g.open()
	requireHelperWrote(t, id, orig)
	mustSh(t, "install", "-m", "0600", "-o", "root", other, nexusdConfig)
	g.cont()
	r := g.awaitHeld(id, orig, tOpen)
	requireOutcome(t, r.op, problem.ConfigConflict)
	if s := sha(t, nexusdConfig); s != otherSHA {
		t.Errorf("sha256(file) %s, want the replacement's %s (not restored)", s, otherSHA)
	}
	st, ok := raStatus(t)
	if !ok || !st.Applied || st.SHA256 != otherSHA {
		t.Errorf("reload-status.json %+v, want applied with sha256 %s", st, otherSHA)
	}
	requirePC(t, r.op)
}

// TC-RA-ERR36 (WI 5.2; FR-8, NFR-6, NFR-2): a restore that cannot be
// confirmed ends config-restore-failed, and the daemon later loads the
// restored file.
func TestTCRAERR36_OnRealSystemdARestoreThatCannotBeConfirmedEnds(t *testing.T) {
	h := newRAHost(t)
	orig := sha(t, nexusdConfig)
	g := h.held()
	id := g.gate("providers.host.enabled=true")
	g.stop()
	tOpen := g.open()
	requireHelperWrote(t, id, orig)
	exitJSON := filepath.Join(smOpsDir, id, "exit.json")
	deadline := time.Now().Add(raExitBound)
	for !exists(exitJSON) {
		if time.Now().After(deadline) {
			j, _ := sh(30*time.Second, "journalctl", "-u", detach.Unit(id), "--no-pager", "-o", "cat")
			t.Fatalf("%s did not appear within %s: both attempts did not expire\n%s", exitJSON, raExitBound, j)
		}
		time.Sleep(raDigestPoll)
	}
	unitEnded := time.Since(tOpen)
	t.Logf("exit.json %s after tOpen", unitEnded.Round(time.Millisecond))
	if unitEnded > raMaxRuntime {
		t.Errorf("the unit ended %s after the gate opened, over MaxRuntime %s", unitEnded, raMaxRuntime)
	}
	g.cont()
	r := g.awaitHeld(id, orig, tOpen)
	requireOutcome(t, r.op, problem.ConfigRestoreFailed)
	if v := errExt(r.op, "backupPath"); v != raBackup {
		t.Errorf("backupPath %q, want %s", v, raBackup)
	}
	if d := r.op.Error.Detail; !strings.Contains(d, "docs/install.md#") {
		t.Errorf("detail %q does not name the docs/install.md recovery anchor", d)
	}
	if s := sha(t, nexusdConfig); s != orig {
		t.Errorf("sha256(file) %s, want the original %s: the backup bytes were written back", s, orig)
	}
	waitUntil(t, 10*time.Second, "a reload status applying the restored file", func() (bool, string) {
		st, ok := raStatus(t)
		return ok && st.Applied && st.SHA256 == orig, fmt.Sprintf("%+v", st)
	})
	nfrLine(t, "config set whose restore was not confirmed (TC-RA-ERR36), tOpen to ended", r.took, raRestoreBudget)
	g.release()
}

// TC-RA-ERR39 (WI 5.2; NFR-2, FR-8): a refused reload during a held window is
// restored within the NFR-2 budget, measured from the gate's opening.
func TestTCRAERR39_OnRealSystemdARefusedReloadDuringAHeldWindowIs(t *testing.T) {
	h := newRAHost(t)
	r := h.heldRefusedRestore("providers.host.enabled=true")
	requireRestored(t, r)
	nfrLine(t, "config set refused at load and restored (TC-RA-ERR39), tOpen to ended", r.took, raRestoreBudget)
}

// TC-RA-E28 (WI 5.2; NFR-6, CON-1): the hold drop-in applies to nexus-op
// units and never to nexusd.service (A-34).
func TestTCRAE28_TheHoldDropInAppliesToNexusOpUnitsAndNever(t *testing.T) {
	h := newRAHost(t)
	g := h.held()
	if p := showUnit(t, nexusdUnit, "DropInPaths")["DropInPaths"]; p != "" {
		t.Errorf("step 1: nexusd.service DropInPaths %q, want none while the seam is armed", p)
	}
	requireExactUnit(t)
	id := g.gate("providers.host.enabled=true")
	p := showUnit(t, detach.Unit(id), "DropInPaths", "ActiveState", "SubState")
	if !slices.Contains(strings.Fields(p["DropInPaths"]), raHoldDropIn) || p["ActiveState"] != "activating" ||
		p["SubState"] != "start-pre" {
		t.Errorf("step 3: %s %v, want DropInPaths listing %s, activating/start-pre", detach.Unit(id), p, raHoldDropInFn)
	}
	if res := filepath.Join(smOpsDir, id, "result.json"); exists(res) {
		t.Errorf("step 3: %s exists while the gate is closed", res)
	}
	g.open()
	op := h.awaitOp(id, raOpBound)
	requireSucceeded(t, op)
	requirePC(t, op)
	g.release()
	if p := showUnit(t, nexusdUnit, "DropInPaths")["DropInPaths"]; p != "" {
		t.Errorf("step 5: nexusd.service DropInPaths %q, want none", p)
	}
	if exists(raHoldDropIn) {
		t.Errorf("step 5: %s still exists", raHoldDropIn)
	}
	requireExactUnit(t)
}

// TC-RA-ERR44 (WI 5.2; FR-7, NFR-6): a file changed between admission and
// the helper ends config-conflict, with nothing edited, backed up or
// reloaded.
func TestTCRAERR44_OnRealSystemdAFileChangedBetweenAdmissionAnd(t *testing.T) {
	h := newRAHost(t)
	other, otherSHA := raOther(t)
	before := fileState(t)
	g := h.held()
	id := g.gate("providers.host.enabled=true")
	// Prepare has run: the unit exists and waits in ExecStartPre=.
	mustSh(t, "install", "-m", "0600", "-o", "root", other, nexusdConfig)
	g.open()
	op := h.awaitOp(id, raOpBound)
	requireOutcome(t, op, problem.ConfigConflict)
	if s := sha(t, nexusdConfig); s != otherSHA {
		t.Errorf("sha256(file) %s, want the test's content %s", s, otherSHA)
	}
	if b := exists(raBackup); b != before.bak || (b && sha(t, raBackup) != before.bakSHA) {
		t.Errorf("%s present %t, want %t with sha256 %q unchanged", raBackup, b, before.bak, before.bakSHA)
	}
	if st, _ := raStatus(t); st.Seq != before.seq {
		t.Errorf("reload-status.json seq %d, want %d: the helper sent a reload", st.Seq, before.seq)
	}
	// The supervisor captures the helper's stdout and stderr into the
	// operation's log; the unit's journal has only systemd's own lines.
	// Both are read: the conflict step is in the log, and neither shows a
	// reload.
	log := h.opLog(id)
	j := mustSh(t, "journalctl", "-u", detach.Unit(id), "--no-pager", "-o", "cat")
	t.Logf("operation log of %s:\n%s\njournalctl -u %s:\n%s", id, log, detach.Unit(id), j)
	if !strings.Contains(log, "changed since the request was admitted") {
		t.Errorf("the operation log of %s lacks the helper's conflict step", id)
	}
	for what, text := range map[string]string{"the operation log": log, "journalctl -u " + detach.Unit(id): j} {
		if strings.Contains(text, "systemctl reload") {
			t.Errorf("%s shows a reload", what)
		}
	}
}

// raFaultPatterns are TC-RA-103's denial and crash patterns.
var raFaultPatterns = []string{"Permission denied", "EACCES", "EROFS", "audit: type=1400", "code=dumped"}

// TC-RA-103 (WI 5.2; CON-1, NFR-6): no sandbox denial, crash or automatic
// restart during the T1 run. It runs last in the file, over the window
// since the first TestTCRA host started.
func TestTCRA103_NoSandboxDenialCrashOrRestartDuringTheT1Run(t *testing.T) {
	requireInit(t, platform.Systemd)
	raRun.Lock()
	first := raRun.first
	raRun.Unlock()
	if first.IsZero() {
		// Run on its own: make the window non-empty with one config set.
		h := newRAHost(t)
		h.configSetHostEnabled()
		raRun.Lock()
		first = raRun.first
		raRun.Unlock()
	}
	since := "@" + strconv.FormatInt(first.Unix(), 10)
	reloaded := 0
	for _, e := range raJournal(t, since, nexusdUnit, detach.UnitPrefix+"*") {
		if strings.HasPrefix(e.msg, "Reloaded nexusd.service") {
			reloaded++
		}
		if strings.HasPrefix(e.msg, "Scheduled restart job") {
			t.Errorf("journal %s: an automatic restart: %s", e.unit, e.msg)
		}
		for _, p := range raFaultPatterns {
			if strings.Contains(e.msg, p) {
				t.Errorf("journal %s at %s: %q matches %q", e.unit, e.at.Format(time.RFC3339Nano), e.msg, p)
			}
		}
	}
	// The kernel log of the window: only lines about this run's processes
	// count, since the kernel is shared with the Docker host.
	kernel, err := sh(time.Minute, "journalctl", "-k", "--since", since, "--no-pager", "-o", "cat")
	if err != nil {
		t.Logf("journalctl -k: %v %s", err, kernel)
	}
	for line := range strings.Lines(kernel) {
		ours := strings.Contains(line, "nexus")
		for _, c := range sandboxComms {
			ours = ours || strings.Contains(line, c)
		}
		for _, p := range raFaultPatterns {
			if ours && strings.Contains(line, p) {
				t.Errorf("kernel log: %q matches %q", strings.TrimSpace(line), p)
			}
		}
	}
	n := showUnit(t, nexusdUnit, "NRestarts")["NRestarts"]
	t.Logf("window since %s: %d Reloaded lines; nexusd.service NRestarts %s", first.Format(time.RFC3339), reloaded, n)
	if reloaded == 0 {
		t.Error("non-vacuity: the window has no Reloaded line")
	}
	if n != "0" {
		t.Errorf("NRestarts %s, want 0: no automatic restart in the run", n)
	}
}

// sha256Sum is the hex SHA-256 of b, as reload-status.json records it.
func sha256Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
