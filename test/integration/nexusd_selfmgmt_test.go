//go:build integration

package integration

// WI 7.2: nexusd self-management on real systemd, the feature's T1 matrix
// (TC-SM-109 to TC-SM-129). Every case starts from a wiped host, installs
// the v9.9.1 fixture with its own install.sh, configures the nexusd
// provider with the per-run release key run.sh made (WI 7.1) and TLS auto,
// starts nexusd.service under the shipped unit, mints a token with
// nexusd:read and nexusd:admin, and drives everything with this binary as
// the nexus CLI over the pinned certificate. Cleanup waits for every
// nexus-op-* unit to end and puts back the harness install.
//
// Expected values come from systemctl show, the binaries' own -version,
// the files, the journal and the audit log, never only from Nexus. Every
// update and rollback is also watched from outside by an observer that
// samples nexusd's MainPID, the nexus-op-* units and every operation's
// status, so "never outcome-unknown" (FR-9) and "the op unit survives the
// nexusd restart" (A-19) are read back rather than assumed. The NFR
// figures are logged on lines starting with NFR for
// docs/verification/measurements.md. Nothing here names any host but
// 127.0.0.1 (CON-1).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

const (
	smPrev           = nexusdBin + ".prev"
	smSavedUnit      = nexusdState + "/rollback/" + nexusdUnit
	smStaging        = nexusdState + "/staging"
	smOpsDir         = nexusdState + "/ops"
	smAudit          = logDir + "/audit.jsonl"
	smPubKey         = selfupdateDir + "/release-test.pub"
	smUploadsAt      = "/v1/providers/nexusd/uploads"
	smDaemonsAt      = "/v1/providers/nexusd/daemons"
	smActionsAt      = "/v1/providers/nexusd/actions/"
	smRecoveryAnchor = "docs/install.md#recovering-from-a-failed-update"
	// smOpMaxRuntime bounds an update op: install 120 s, two health phases
	// of healthTimeout 30 s plus 20 s each, and 60 s (update.go).
	smOpMaxRuntime = 280 * time.Second
	// smRollbackBound is NFR-1's bound on a rollback case: healthTimeout
	// (30 s) + 20 s.
	smRollbackBound = 50 * time.Second
)

// smTarball is fixture version v's signed tarball; its signature is
// smTarball(v)+".sig".
func smTarball(v string) string {
	return filepath.Join(selfupdateDir, "nexusd-"+v+"-linux-"+os.Getenv("NEXUS_HARNESS_ARCH")+".tar.gz")
}

// smConfig is the case's /etc/nexus/nexusd.yaml: loopback, TLS auto, the
// nexusd provider enabled with the per-run key trusted, and extra
// top-level sections.
func smConfig(pub, extra string) string {
	return fmt.Sprintf("listen: [%q]\ntls: {mode: auto}\n%sproviders:\n  nexusd:\n    enabled: true\n    update:\n"+
		"      trustedKeys: [%q]\n", exactAddr, extra, pub)
}

// smWipe waits for every nexus-op-* unit to end, then removes every trace
// of an install, nexusd.prev and the swap copies included.
func smWipe(t *testing.T) {
	t.Helper()
	waitOpUnitsIdle(t, smOpMaxRuntime+time.Minute)
	wipeInstall(t)
	for _, p := range []string{smPrev, smPrev + ".tmp", nexusdBin + ".swap.tmp", nexusdBin + ".new",
		installedUnit + ".swap.new.tmp", smPrevReal} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

// smOpUnits returns the nexus-op-* units systemd knows, with their
// ActiveState.
func smOpUnits() map[string]string {
	out, err := sh(30*time.Second, detach.Systemctl, "list-units", "--all", "--plain", "--no-legend", "--no-pager",
		detach.UnitPrefix+"*")
	m := map[string]string{}
	if err != nil {
		return m
	}
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) >= 3 && strings.HasPrefix(f[0], detach.UnitPrefix) {
			m[f[0]] = f[2]
		}
	}
	return m
}

// waitOpUnitsIdle waits until no nexus-op-* unit is active or activating,
// so a case never leaves a helper running into the next one.
func waitOpUnitsIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, "every nexus-op-* unit to end", func() (bool, string) {
		var busy []string
		for u, st := range smOpUnits() {
			if st == "active" || st == "activating" || st == "deactivating" {
				busy = append(busy, u+"="+st)
			}
		}
		return len(busy) == 0, fmt.Sprint(busy)
	})
}

// smHost is one T1 case's host: v9.9.1 installed by install.sh, running.
type smHost struct {
	t     *testing.T
	pub   string // The per-run public key, trusted in the config
	token string // nexusd:read and nexusd:admin
	fp    string // The pinned certificate fingerprint
	home  string // The CLI's HOME
	http  *http.Client
	pid0  int // MainPID after the start
}

// newSMHost prepares the clean v9.9.1 host of a case. name names the token.
func newSMHost(t *testing.T, name string) *smHost {
	t.Helper()
	smWipe(t)
	t.Cleanup(func() {
		smWipe(t)
		restoreHarness(t)
	})
	b, err := os.ReadFile(smPubKey)
	if err != nil {
		t.Fatalf("the per-run public key: %v (run.sh writes it, WI 7.1)", err)
	}
	h := &smHost{t: t, pub: strings.TrimSpace(string(b))}
	if out := installTree(t, unpackFixture(t, "v9.9.1")); !strings.Contains(out, "installed nexusd v9.9.1") {
		t.Fatalf("install.sh from v9.9.1 did not report the install:\n%s", out)
	}
	writeConfig(t, smConfig(h.pub, ""))
	h.pid0 = startExact(t)
	h.token = createToken(t, name, spec.ReadScope, spec.AdminScope)
	h.fp = client.Fingerprint(readCert(t, filepath.Join(nexusdState, "tls", "cert.pem")).Raw)
	tc, err := client.PinnedTLS(h.fp)
	if err != nil {
		t.Fatal(err)
	}
	h.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	h.home = h.cliHome(h.token)
	t.Logf("T1 host: %s from install.sh, nexusd.service MainPID %d, certificate %s", binVersion(t, nexusdBin), h.pid0,
		h.fp)
	return h
}

// cliHome writes a CLI configuration for token in a fresh HOME.
func (h *smHost) cliHome(token string) string {
	h.t.Helper()
	home := h.t.TempDir()
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	conf := fmt.Sprintf("defaultHost: t1\nhosts:\n  t1:\n    url: https://%s\n    fingerprint: %q\n    token: %q\n",
		exactAddr, h.fp, token)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(conf), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return home
}

// smRun is one nexus CLI run.
type smRun struct {
	args           []string
	code           int
	stdout, stderr string
	took           time.Duration
}

// cli runs this binary as the nexus CLI with h's admin token, no
// terminal and an empty stdin.
func (h *smHost) cli(args ...string) smRun { return h.cliAs(h.home, args...) }

func (h *smHost) cliAs(home string, args ...string) smRun {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = pipelineEnv(home)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	start := time.Now()
	err := cmd.Run()
	r := smRun{args: args, stdout: out.String(), stderr: errb.String(), took: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		h.t.Fatalf("nexus %s: %v", strings.Join(args, " "), err)
	}
	h.t.Logf("nexus %s: exit %d in %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code,
		r.took.Round(time.Millisecond), strings.TrimSpace(r.stdout), strings.TrimSpace(r.stderr))
	return r
}

// ops decodes the operations a machine-format run printed on stdout.
func (r smRun) ops() []apitypes.Operation {
	s := strings.TrimSpace(r.stdout)
	var ops []apitypes.Operation
	if json.Unmarshal([]byte(s), &ops) == nil {
		return ops
	}
	ops = nil
	for line := range strings.Lines(s) {
		var op apitypes.Operation
		if json.Unmarshal([]byte(line), &op) == nil && op.ID != "" {
			ops = append(ops, op)
		}
	}
	return ops
}

// problem is the last JSON error line on stderr, or nil.
func (r smRun) problem() map[string]any {
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			if _, ok := m["type"]; ok {
				return m
			}
		}
	}
	return nil
}

var opGetRE = regexp.MustCompile(`operation get ([A-Za-z0-9_-]+)`)

// opID is the operation the run started: from stdout, else the failed
// operation's operationId on the error line, else the `operation get <id>`
// an error names.
func (r smRun) opID() string {
	if ops := r.ops(); len(ops) > 0 {
		return ops[0].ID
	}
	if id, ok := r.problem()["operationId"].(string); ok && id != "" {
		return id
	}
	if m := opGetRE.FindStringSubmatch(r.stderr); m != nil {
		return m[1]
	}
	return ""
}

// smSlug is the slug of a problem map's type URI.
func smSlug(m map[string]any) string {
	typ, _ := m["type"].(string)
	return strings.TrimPrefix(typ, problem.TypePrefix)
}

// rest sends one request over the pinned connection with h's token. A
// transport failure is an error, not fatal: nexusd may be restarting.
func (h *smHost) rest(method, path string, body io.Reader, ctype string) (int, []byte, error) {
	return h.restAs(h.token, method, path, body, ctype)
}

func (h *smHost) restAs(token, method, path string, body io.Reader, ctype string) (int, []byte, error) {
	req, err := http.NewRequest(method, "https://"+exactAddr+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (h *smHost) op(id string) (apitypes.Operation, error) {
	code, b, err := h.rest(http.MethodGet, "/v1/operations/"+id, nil, "")
	if err != nil {
		return apitypes.Operation{}, err
	}
	if code != http.StatusOK {
		return apitypes.Operation{}, fmt.Errorf("GET operation %s: %d %s", id, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		return apitypes.Operation{}, err
	}
	return op, nil
}

// mustOp reads operation id and fails on outcome-unknown.
func (h *smHost) mustOp(id string) apitypes.Operation {
	h.t.Helper()
	op, err := h.op(id)
	if err != nil {
		h.t.Fatal(err)
	}
	if problemSlug(op) == string(problem.OutcomeUnknown) {
		h.t.Fatalf("operation %s is outcome-unknown (FR-9): %+v", id, op.Error)
	}
	return op
}

// awaitOp polls operation id through restarts until it is terminal; it
// fails at once on outcome-unknown.
func (h *smHost) awaitOp(id string, timeout time.Duration) apitypes.Operation {
	h.t.Helper()
	var op apitypes.Operation
	waitUntil(h.t, timeout, "operation "+id+" to end", func() (bool, string) {
		o, err := h.op(id)
		if err != nil {
			return false, err.Error()
		}
		if problemSlug(o) == string(problem.OutcomeUnknown) {
			h.t.Fatalf("operation %s is outcome-unknown (FR-9): %+v", id, o.Error)
		}
		op = o
		return terminal(o.Status), string(o.Status)
	})
	return op
}

// smRecoveryBound is FR-6's bound: after a restart an operation reaches a
// terminal state within 60 seconds.
const smRecoveryBound = 60 * time.Second

// pollCLIOp runs `nexus operation get <id> -o json` every 100 ms until the
// operation is terminal, at most smRecoveryBound. nexusd classifies a
// recovered detached operation after it is ready, so a single read right
// after the restart may still see Running (FR-6, W-10). Every read that
// returned the operation is appended to polls as status:problem-slug, so the
// caller's never-outcome-unknown check covers all of them. It returns the
// terminal operation and the run that read it.
func (h *smHost) pollCLIOp(id string, polls *[]string) (apitypes.Operation, smRun) {
	h.t.Helper()
	var op apitypes.Operation
	var run smRun
	waitUntil(h.t, smRecoveryBound, "operation "+id+" to be terminal", func() (bool, string) {
		run = h.cli("operation", "get", id, "-o", "json")
		ops := run.ops()
		if run.code != 0 || len(ops) != 1 {
			return false, fmt.Sprintf("exit %d %s", run.code, strings.TrimSpace(run.stdout))
		}
		op = ops[0]
		*polls = append(*polls, string(op.Status)+":"+problemSlug(op))
		return terminal(op.Status), string(op.Status)
	})
	return op, run
}

// requireNoUnknownPolls fails when any poll saw outcome-unknown (FR-9).
func requireNoUnknownPolls(t *testing.T, polls []string) {
	t.Helper()
	for _, p := range polls {
		if strings.Contains(p, string(problem.OutcomeUnknown)) {
			t.Errorf("a poll saw %s (FR-9)", p)
		}
	}
}

// nexusdOps lists the nexusd provider's operations.
func (h *smHost) nexusdOps() []apitypes.Operation {
	h.t.Helper()
	code, b, err := h.rest(http.MethodGet, "/v1/operations?limit=1000", nil, "")
	if err != nil || code != http.StatusOK {
		h.t.Fatalf("list operations: %d %v %s", code, err, b)
	}
	var l apitypes.List[apitypes.Operation]
	if err := json.Unmarshal(b, &l); err != nil {
		h.t.Fatal(err)
	}
	var out []apitypes.Operation
	for _, op := range l.Items {
		if op.Provider == spec.Name {
			out = append(out, op)
		}
	}
	return out
}

// opLog is operation id's whole output.
func (h *smHost) opLog(id string) string {
	h.t.Helper()
	code, b, err := h.rest(http.MethodGet, "/v1/operations/"+id+"/logs", nil, "")
	if err != nil || code != http.StatusOK {
		h.t.Fatalf("logs of %s: %d %v %s", id, code, err, b)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			h.t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out.WriteString(f.D)
	}
	if err := sc.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out.String()
}

// daemon is GET /providers/nexusd/daemons's one item.
func (h *smHost) daemon() spec.DaemonProperties {
	h.t.Helper()
	code, b, err := h.rest(http.MethodGet, smDaemonsAt, nil, "")
	if err != nil || code != http.StatusOK {
		h.t.Fatalf("GET daemons: %d %v %s", code, err, b)
	}
	var l apitypes.List[spec.Daemon]
	if err := json.Unmarshal(b, &l); err != nil || len(l.Items) != 1 {
		h.t.Fatalf("GET daemons: %v %s", err, b)
	}
	return l.Items[0].Properties
}

// unitPID is nexusd.service's MainPID, 0 when it has none.
func unitPID(t *testing.T) int {
	t.Helper()
	pid, err := strconv.Atoi(showUnit(t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// smWindow opens a journal window on the next whole second, after every
// line already logged, and returns its --since argument.
func smWindow() string {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	return "@" + strconv.FormatInt(time.Now().Unix(), 10)
}

// unitJournal is unit's journal since since, message text only.
func unitJournal(t *testing.T, unit, since string) string {
	t.Helper()
	return mustSh(t, "journalctl", "-u", unit, "-o", "cat", "--no-pager", "--since", since)
}

// startedLines are the Started lines of nexusd.service's journal since
// since.
func startedLines(t *testing.T, since string) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(unitJournal(t, nexusdUnit, since)) {
		if strings.HasPrefix(line, "Started ") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// stagingEntries lists /var/lib/nexus/staging.
func stagingEntries(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(smStaging)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func requireStagingEmpty(t *testing.T, when string) {
	t.Helper()
	if es := stagingEntries(t); len(es) != 0 {
		t.Errorf("%s: %s holds %q, want it empty", when, smStaging, es)
	} else {
		t.Logf("%s: %s is empty", when, smStaging)
	}
}

// decodeResult decodes an operation's result into a T.
func decodeResult[T any](t *testing.T, op apitypes.Operation) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(op.Result, &v); err != nil {
		t.Fatalf("result of %s: %v: %s", op.ID, err, op.Result)
	}
	return v
}

// errExt is an extension member of an operation's error, as text.
func errExt(op apitypes.Operation, key string) string {
	if op.Error == nil {
		return ""
	}
	return fmt.Sprint(op.Error.Extensions[key])
}

// smSample is one observer tick.
type smSample struct {
	at        time.Time
	nexusdPID int
	units     map[string]string // nexus-op-* unit -> "ActiveState MainPID"
}

// smObservation is what an observer saw.
type smObservation struct {
	statuses map[string][]string // Operation ID -> its statuses in order, with the problem slug when failed
	samples  []smSample
}

// observe samples nexusd.service's MainPID, the nexus-op-* units and the
// nexusd operations' statuses every 100 ms until stop is called. It runs
// no t method off the test goroutine.
func (h *smHost) observe() (stop func() smObservation) {
	obs := smObservation{statuses: map[string][]string{}}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			s := smSample{at: time.Now(), units: map[string]string{}}
			if out, err := sh(10*time.Second, detach.Systemctl, "show", nexusdUnit, "-p", "MainPID", "--value"); err == nil {
				s.nexusdPID, _ = strconv.Atoi(strings.TrimSpace(out))
			}
			for u := range smOpUnits() {
				if out, err := sh(10*time.Second, detach.Systemctl, "show", u, "-p", "ActiveState", "-p", "MainPID",
					"--value"); err == nil {
					s.units[u] = strings.Join(strings.Fields(out), " ")
				}
			}
			obs.samples = append(obs.samples, s)
			if code, b, err := h.rest(http.MethodGet, "/v1/operations?limit=1000", nil, ""); err == nil && code == http.StatusOK {
				var l apitypes.List[apitypes.Operation]
				if json.Unmarshal(b, &l) == nil {
					for _, op := range l.Items {
						if op.Provider != spec.Name {
							continue
						}
						st := string(op.Status)
						if slug := problemSlug(op); slug != "" {
							st += ":" + slug
						}
						if seen := obs.statuses[op.ID]; len(seen) == 0 || seen[len(seen)-1] != st {
							obs.statuses[op.ID] = append(seen, st)
						}
					}
				}
			}
			select {
			case <-quit:
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	return func() smObservation {
		once.Do(func() { close(quit) })
		<-done
		return obs
	}
}

// requireNeverUnknown fails when any operation was seen outcome-unknown.
func requireNeverUnknown(t *testing.T, obs smObservation) {
	t.Helper()
	for id, sts := range obs.statuses {
		t.Logf("observer: operation %s went %s", id, strings.Join(sts, " -> "))
		for _, s := range sts {
			if strings.Contains(s, string(problem.OutcomeUnknown)) {
				t.Errorf("operation %s passed through %s (FR-9)", id, s)
			}
		}
	}
}

// opUnitSurvived is A-19 read back: the op's unit was active with one
// MainPID while nexusd ran as pid0, and still active with that MainPID once
// nexusd ran as a new process.
func opUnitSurvived(obs smObservation, id string, pid0 int) (string, bool) {
	unit := detach.Unit(id)
	var helper string
	for _, s := range obs.samples {
		st := strings.Fields(s.units[unit])
		if len(st) != 2 || st[0] != "active" {
			continue
		}
		switch {
		case s.nexusdPID == pid0:
			helper = st[1]
		case helper != "" && s.nexusdPID > 0 && st[1] == helper:
			return fmt.Sprintf("%s active with MainPID %s while nexusd ran as %d, and at %s while nexusd ran as %d",
				unit, helper, pid0, s.at.Format(time.RFC3339Nano), s.nexusdPID), true
		}
	}
	return fmt.Sprintf("%s: no sample active across the nexusd restart (helper PID %q)", unit, helper), false
}

var (
	restartStartedRE = regexp.MustCompile(`restart-started (\d{4}-\d\d-\d\dT\S+)`)
	healthyRE        = regexp.MustCompile(`\shealthy (\d{4}-\d\d-\d\dT\S+)`)
)

// nfr1Times parses the NFR-1 lines of an update's log: the first and last
// restart-started and the last healthy.
func nfr1Times(t *testing.T, log string) (first, last, healthy time.Time) {
	t.Helper()
	parse := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("timestamp %q: %v", s, err)
		}
		return v
	}
	rs := restartStartedRE.FindAllStringSubmatch(log, -1)
	hs := healthyRE.FindAllStringSubmatch(log, -1)
	if len(rs) == 0 || len(hs) == 0 {
		t.Fatalf("the op log lacks restart-started or healthy lines:\n%s", log)
	}
	return parse(rs[0][1]), parse(rs[len(rs)-1][1]), parse(hs[len(hs)-1][1])
}

// smUpdate is one waited `nexus nexusd update` from the CLI, watched by
// an observer, with the op read back.
type smUpdate struct {
	run  smRun
	op   apitypes.Operation
	obs  smObservation
	log  string
	pid1 int // nexusd.service's MainPID after the op ended
}

func (h *smHost) update(v string, extra ...string) smUpdate {
	h.t.Helper()
	stop := h.observe()
	args := append([]string{"nexusd", "update", "--file", smTarball(v), "-o", "json", "--verbose"}, extra...)
	run := h.cli(args...)
	id := run.opID()
	if id == "" {
		stop()
		h.t.Fatalf("update to %s started no operation (exit %d)", v, run.code)
	}
	op := h.awaitOp(id, smOpMaxRuntime)
	obs := stop()
	requireNeverUnknown(h.t, obs)
	u := smUpdate{run: run, op: op, obs: obs, log: h.opLog(id), pid1: unitPID(h.t)}
	h.t.Logf("update to %s: operation %s %s, error %v, result %s; nexusd.service MainPID %d -> %d\nop log:\n%s", v, id,
		op.Status, op.Error, op.Result, h.pid0, u.pid1, u.log)
	return u
}

// requireUploaded checks the CLI's --verbose log for the upload answered
// 201.
func requireUploaded(t *testing.T, r smRun) {
	t.Helper()
	lines := strings.Split(r.stderr, "\n")
	for i, l := range lines {
		if strings.Contains(l, "> POST ") && strings.Contains(l, smUploadsAt) {
			if i+1 < len(lines) && strings.Contains(lines[i+1], "< 201 ") {
				t.Logf("upload: %s | %s", strings.TrimSpace(l), strings.TrimSpace(lines[i+1]))
				return
			}
		}
	}
	t.Errorf("the CLI's --verbose log shows no upload answered 201:\n%s", r.stderr)
}

// requireOpUnitRan checks that the op's transient unit logged something.
func requireOpUnitRan(t *testing.T, id string) {
	t.Helper()
	j := mustSh(t, "journalctl", "-u", detach.Unit(id), "-o", "cat", "--no-pager")
	if strings.TrimSpace(j) == "" || strings.Contains(j, "-- No entries --") {
		t.Errorf("journalctl -u %s is empty", detach.Unit(id))
	} else {
		t.Logf("journalctl -u %s: %d lines", detach.Unit(id), strings.Count(j, "\n"))
	}
}

// requireActive checks nexusd.service is active and returns its state.
func requireActive(t *testing.T, when string) map[string]string {
	t.Helper()
	p := showUnit(t, nexusdUnit, "ActiveState", "SubState", "Result", "MainPID", "NRestarts")
	t.Logf("%s: nexusd.service %v", when, p)
	if p["ActiveState"] != "active" {
		t.Errorf("%s: nexusd.service is %s, want active", when, p["ActiveState"])
	}
	return p
}

// TC-SM-109 (WI 7.2; FR-7, FR-9, FR-10, FR-14, NFR-6; A-17, A-19, A-20,
// A-31, A-33): T1 (a): an update from v9.9.1 to v9.9.2 succeeds with
// exactly one restart.
func TestTCSM109_T1AAnUpdateFromV9(t *testing.T) {
	h := newSMHost(t, "tcsm109")
	unitBefore, err := os.ReadFile(installedUnit)
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Count(mustSh(t, "tar", "-tzf", smTarball("v9.9.2")), "\n")
	if entries <= 64 {
		t.Fatalf("%s has %d entries, want more than 64 (a real make nexusd-tree tree, TC-SM-108)", smTarball("v9.9.2"), entries)
	}
	since := smWindow()

	u := h.update("v9.9.2")
	if u.run.code != 0 {
		t.Fatalf("exit %d, want 0", u.run.code)
	}
	requireUploaded(t, u.run)
	if u.op.Action != spec.Update.Op().ID || u.op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %s, want a Succeeded %s (not refused update-package-invalid)", u.op.Action, u.op.Status,
			spec.Update.Op().ID)
	}
	res := decodeResult[spec.UpdateResult](t, u.op)
	if res.PreviousVersion != "v9.9.1" || res.NewVersion != "v9.9.2" || !res.Changed || res.RolledBack {
		t.Errorf("result %s, want previousVersion v9.9.1, newVersion v9.9.2, changed, not rolledBack", u.op.Result)
	}
	// Read-backs from the files.
	if got := binVersion(t, nexusdBin); got != "nexusd v9.9.2" {
		t.Errorf("%s -version %q, want nexusd v9.9.2", nexusdBin, got)
	}
	if got := binVersion(t, smPrev); got != "nexusd v9.9.1" {
		t.Errorf("%s -version %q, want nexusd v9.9.1", smPrev, got)
	}
	if saved, err := os.ReadFile(smSavedUnit); err != nil || !bytes.Equal(saved, unitBefore) {
		t.Errorf("%s does not equal the pre-update unit (%v)", smSavedUnit, err)
	}
	// Exactly one start of nexusd.service since the window opened (A-33),
	// onto a new process running the new image (A-17).
	started := startedLines(t, since)
	p := requireActive(t, "after the update")
	t.Logf("journal since %s: Started lines %q", since, started)
	if len(started) != 1 {
		t.Errorf("%d Started lines since the update began, want exactly 1: %q", len(started), started)
	}
	if u.pid1 == h.pid0 || u.pid1 <= 0 {
		t.Errorf("MainPID %d after the update, want a new one (was %d)", u.pid1, h.pid0)
	}
	if got := binVersion(t, fmt.Sprintf("/proc/%d/exe", u.pid1)); got != "nexusd v9.9.2" {
		t.Errorf("/proc/%d/exe -version %q, want nexusd v9.9.2 (A-17)", u.pid1, got)
	}
	// The op log: the backup from /proc/<pid0>/exe (A-20), install.sh's
	// own output and its restart (A-33).
	for _, want := range []string{
		fmt.Sprintf("backup: %s (nexusd v9.9.1, from /proc/%d/exe)", smPrev, h.pid0),
		"installed nexusd v9.9.2",
		"restarted the running nexusd.service onto the new binary",
		fmt.Sprintf("install.sh restarted %s (MainPID %s, was %d)", nexusdUnit, p["MainPID"], h.pid0),
	} {
		if !strings.Contains(u.log, want) {
			t.Errorf("the op log lacks %q", want)
		}
	}
	// A-31: admission executed the staged package's nexusd -version from
	// inside the shipped sandbox, or no operation would exist.
	eff := showUnit(t, nexusdUnit, "ProtectSystem", "NoNewPrivileges", "MemoryDenyWriteExecute", "SystemCallFilter",
		"CapabilityBoundingSet", "DropInPaths")
	t.Logf("A-31: the update was admitted (package nexusd -version probed) by nexusd under ProtectSystem=%s "+
		"NoNewPrivileges=%s MemoryDenyWriteExecute=%s CapabilityBoundingSet=%s, drop-ins %q", eff["ProtectSystem"],
		eff["NoNewPrivileges"], eff["MemoryDenyWriteExecute"], eff["CapabilityBoundingSet"], eff["DropInPaths"])
	if eff["ProtectSystem"] != "strict" || eff["NoNewPrivileges"] != "yes" || eff["DropInPaths"] != "" {
		t.Errorf("A-31: nexusd.service is not the shipped sandbox: %v", eff)
	}
	requireStagingEmpty(t, "after the update")
	requireOpUnitRan(t, u.op.ID)
	// A-19: the helper's unit kept running across the nexusd restart.
	if how, ok := opUnitSurvived(u.obs, u.op.ID, h.pid0); ok {
		t.Logf("A-19: %s", how)
	} else {
		t.Errorf("A-19 not shown: %s", how)
	}
	first, _, healthy := nfr1Times(t, u.log)
	t.Logf("NFR NSM-NFR-1 TC-SM-109 update v9.9.1->v9.9.2: restart-started %s -> healthy %s = %s",
		first.Format(time.RFC3339Nano), healthy.Format(time.RFC3339Nano), healthy.Sub(first))
}

// rollbackCase updates a clean host to v and requires an automatic
// rollback: exit 1, update-failed, rolledBack, v9.9.1 running and active.
func rollbackCase(t *testing.T, name, v string) (*smHost, smUpdate) {
	t.Helper()
	h := newSMHost(t, name)
	u := h.update(v)
	if u.run.code != 1 {
		t.Errorf("exit %d, want 1", u.run.code)
	}
	if u.op.Status != apitypes.StatusFailed || problemSlug(u.op) != string(problem.UpdateFailed) ||
		errExt(u.op, "rolledBack") != "true" {
		t.Fatalf("operation %s %s %v, want Failed update-failed with rolledBack true", u.op.ID, u.op.Status, u.op.Error)
	}
	if got := binVersion(t, nexusdBin); got != "nexusd v9.9.1" {
		t.Errorf("%s -version %q after the rollback, want nexusd v9.9.1", nexusdBin, got)
	}
	if got := binVersion(t, fmt.Sprintf("/proc/%d/exe", u.pid1)); got != "nexusd v9.9.1" {
		t.Errorf("/proc/%d/exe -version %q, want nexusd v9.9.1", u.pid1, got)
	}
	requireActive(t, "after the rollback")
	requireStagingEmpty(t, "after the rollback")
	return h, u
}

// TC-SM-110 (WI 7.2; FR-8, FR-9): T1 (b): a failing install.sh is rolled
// back.
func TestTCSM110_T1BAFailingInstallSh(t *testing.T) {
	h, u := rollbackCase(t, "tcsm110", "v9.9.3")
	// startExact required the installed unit to be the shipped one before
	// the update; the backup saved that unit.
	unitBefore, err := os.ReadFile(shippedUnit)
	if err != nil {
		t.Fatal(err)
	}
	if step := errExt(u.op, "step"); step != "install" {
		t.Errorf("step %q, want install", step)
	}
	if !strings.Contains(u.op.Error.Detail, "exit code 3") {
		t.Errorf("detail %q does not name exit code 3", u.op.Error.Detail)
	}
	for _, f := range []string{installedUnit, smSavedUnit} {
		if b, err := os.ReadFile(f); err != nil || !bytes.Equal(b, unitBefore) {
			t.Errorf("%s differs from the original unit after the rollback (%v)", f, err)
		}
	}
	if u.pid1 == h.pid0 {
		t.Errorf("MainPID %d after the rollback, the same as during install", u.pid1)
	}
}

// TC-SM-111 (WI 7.2; FR-8, FR-9, FR-10): T1 (c): a binary that exits at
// start is rolled back after reset-failed.
func TestTCSM111_T1CABinaryThatExits(t *testing.T) {
	since := smWindow()
	_, u := rollbackCase(t, "tcsm111", "v9.9.4")
	p := showUnit(t, nexusdUnit, "ActiveState", "Result", "NRestarts")
	if p["ActiveState"] != "active" || p["Result"] != "success" {
		t.Errorf("nexusd.service %v after recovery, want active with Result success", p)
	}
	j := unitJournal(t, nexusdUnit, since)
	if !strings.Contains(j, "nexusd stub v9.9.4: 'serve' fails on purpose") {
		t.Errorf("the journal lacks the v9.9.4 stub's serve failure:\n%s", j)
	}
	t.Logf("nexusd.service journal since %s:\n%s", since, j)
	if !strings.Contains(u.log, "reset-failed") {
		t.Errorf("the op log has no reset-failed step line (TC-SM-111):\n%s", u.log)
	}
}

// TC-SM-112 (WI 7.2; FR-8, FR-9, FR-10; A-17): T1 (d): a healthy binary
// that reports the wrong version is rolled back.
func TestTCSM112_T1DAHealthyBinaryThat(t *testing.T) {
	h := newSMHost(t, "tcsm112")
	// A-17 directly: once another image is renamed over the binary path,
	// /proc/<MainPID>/exe still runs the image the process started from.
	v2 := unpackFixture(t, "v9.9.2")
	v1 := unpackFixture(t, "v9.9.1")
	swap := func(src string) {
		mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", src, nexusdBin+".new")
		mustSh(t, "mv", "-f", nexusdBin+".new", nexusdBin)
	}
	swap(filepath.Join(v2, "nexusd"))
	exe := fmt.Sprintf("/proc/%d/exe", h.pid0)
	gotExe, gotBin := binVersion(t, exe), binVersion(t, nexusdBin)
	link, _ := os.Readlink(exe)
	swap(filepath.Join(v1, "nexusd"))
	t.Logf("A-17: with nexusd v9.9.2 renamed over %s, %s -> %q reports %q; %s reports %q", nexusdBin, exe, link,
		gotExe, nexusdBin, gotBin)
	if gotExe != "nexusd v9.9.1" || gotBin != "nexusd v9.9.2" {
		t.Fatalf("A-17: %s -version %q and %s -version %q, want v9.9.1 and v9.9.2", exe, gotExe, nexusdBin, gotBin)
	}

	u := h.update("v9.9.5")
	if u.run.code != 1 || u.op.Status != apitypes.StatusFailed || problemSlug(u.op) != string(problem.UpdateFailed) ||
		errExt(u.op, "rolledBack") != "true" {
		t.Fatalf("exit %d, operation %s %v; want exit 1 and Failed update-failed rolledBack", u.run.code, u.op.Status, u.op.Error)
	}
	if step := errExt(u.op, "step"); step != "health" {
		t.Errorf("step %q, want health", step)
	}
	if d := u.op.Error.Detail; !strings.Contains(d, "v9.9.8") || !strings.Contains(d, "v9.9.5") {
		t.Errorf("detail %q does not name v9.9.8 and v9.9.5", d)
	}
	if got := binVersion(t, nexusdBin); got != "nexusd v9.9.1" {
		t.Errorf("%s -version %q, want nexusd v9.9.1", nexusdBin, got)
	}
	requireActive(t, "after the rollback")
	requireStagingEmpty(t, "after the rollback")
}

// TC-SM-113 (WI 7.2; FR-7, FR-16, NFR-5): T1 (e): a tarball signed by an
// untrusted key is refused, with no op and the upload deleted.
func TestTCSM113_T1EATarballSignedBy(t *testing.T) {
	h := newSMHost(t, "tcsm113")
	r := h.cli("nexusd", "update", "--file", smTarball("v9.9.6"), "-o", "json", "--verbose")
	p := r.problem()
	if r.code != 1 || smSlug(p) != string(problem.UpdateSignatureInvalid) {
		t.Fatalf("exit %d, problem %v; want exit 1 update-signature-invalid", r.code, p)
	}
	requireUploaded(t, r)
	if ops := h.nexusdOps(); len(ops) != 0 {
		t.Errorf("nexusd operations exist: %+v", ops)
	}
	requireStagingEmpty(t, "after the refusal")
	b, err := os.ReadFile(smAudit)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var last systemspec.AuditRecord
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	t.Logf("last audit record: %s", lines[len(lines)-1])
	if last.Outcome != "refused" || last.Problem != string(problem.UpdateSignatureInvalid) ||
		last.Facts["signature"] != "invalid" || last.Action != spec.Update.Op().ID {
		t.Errorf("last audit record %+v, want the update refused update-signature-invalid with facts.signature invalid",
			last)
	}
	if pid := unitPID(t); pid != h.pid0 {
		t.Errorf("MainPID %d, want unchanged %d", pid, h.pid0)
	}
}

// TC-SM-114 (WI 7.2; FR-5, FR-9, FR-10; A-19): T1 (f): restart produces a
// new MainPID and reports the version.
func TestTCSM114_T1FRestartProducesANew(t *testing.T) {
	h := newSMHost(t, "tcsm114")
	stop := h.observe()
	r := h.cli("nexusd", "restart", "-o", "json")
	obs := stop()
	requireNeverUnknown(t, obs)
	if r.code != 0 || r.opID() == "" {
		t.Fatalf("exit %d, want 0 with the operation", r.code)
	}
	op := h.mustOp(r.opID())
	res := decodeResult[spec.RestartResult](t, op)
	if op.Status != apitypes.StatusSucceeded || res.Version != "v9.9.1" || !res.Changed {
		t.Errorf("operation %s, result %s; want Succeeded {version v9.9.1, changed true}", op.Status, op.Result)
	}
	requireActive(t, "after the restart")
	if pid := unitPID(t); pid == h.pid0 || pid <= 0 {
		t.Errorf("MainPID %d after the restart, want a new one (was %d)", pid, h.pid0)
	}
	if how, ok := opUnitSurvived(obs, op.ID, h.pid0); ok {
		t.Logf("A-19: %s", how)
	} else {
		t.Errorf("A-19 not shown: %s", how)
	}
	var polls []string
	got, g := h.pollCLIOp(op.ID, &polls)
	t.Logf("operation get polls after the restart: %q", polls)
	requireNoUnknownPolls(t, polls)
	if got.Status != apitypes.StatusSucceeded {
		t.Errorf("operation get after the restart: exit %d %s", g.code, g.stdout)
	}
	t.Logf("restart op log:\n%s", h.opLog(op.ID))
}

// TC-SM-115 (WI 7.2; FR-12, FR-9, ASM-7): T1 (g): rollback after (a), then
// rollback again.
func TestTCSM115_T1GRollbackAfterAThen(t *testing.T) {
	h := newSMHost(t, "tcsm115")
	if u := h.update("v9.9.2"); u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded {
		t.Fatalf("setup update to v9.9.2: exit %d, %s", u.run.code, u.op.Status)
	}
	unitSum, savedSum := sha(t, installedUnit), sha(t, smSavedUnit)
	rollback := func(step, from, to string) {
		t.Helper()
		stop := h.observe()
		r := h.cli("nexusd", "rollback", "--yes", "-o", "json")
		requireNeverUnknown(t, stop())
		if r.code != 0 || r.opID() == "" {
			t.Fatalf("%s: exit %d, want 0", step, r.code)
		}
		op := h.mustOp(r.opID())
		res := decodeResult[spec.RollbackResult](t, op)
		t.Logf("%s: %s %s; op log:\n%s", step, op.Status, op.Result, h.opLog(op.ID))
		if op.Status != apitypes.StatusSucceeded || res.FromVersion != from || res.ToVersion != to {
			t.Errorf("%s: %s %s, want Succeeded {fromVersion %s, toVersion %s}", step, op.Status, op.Result, from, to)
		}
		if got := binVersion(t, nexusdBin); got != "nexusd "+to {
			t.Errorf("%s: %s -version %q, want nexusd %s", step, nexusdBin, got, to)
		}
		if got := binVersion(t, smPrev); got != "nexusd "+from {
			t.Errorf("%s: %s -version %q, want nexusd %s", step, smPrev, got, from)
		}
		requireActive(t, step)
	}
	rollback("rollback 1", "v9.9.2", "v9.9.1")
	rollback("rollback 2", "v9.9.1", "v9.9.2")
	if u, s := sha(t, installedUnit), sha(t, smSavedUnit); u != unitSum || s != savedSum {
		t.Errorf("unit files after two rollbacks: %s %s, want the originals %s %s", u, s, unitSum, savedSum)
	}
}

// TC-SM-116 (WI 7.2; FR-12): T1 (h): rollback on a fresh install is
// rollback-unavailable.
func TestTCSM116_T1HRollbackOnAFresh(t *testing.T) {
	h := newSMHost(t, "tcsm116")
	r := h.cli("nexusd", "rollback", "--yes", "-o", "json")
	p := r.problem()
	if r.code != 1 || smSlug(p) != string(problem.RollbackUnavailable) || p["path"] != smPrev {
		t.Errorf("exit %d, problem %v; want exit 1 rollback-unavailable with path %s", r.code, p, smPrev)
	}
	if ops := h.nexusdOps(); len(ops) != 0 {
		t.Errorf("nexusd operations exist: %+v", ops)
	}
	if pid := unitPID(t); pid != h.pid0 {
		t.Errorf("MainPID %d, want unchanged %d", pid, h.pid0)
	}
}

// TC-SM-117 (WI 7.2; FR-4, FR-3): T1 (i): reload refuses an unknown key,
// then applies a valid change.
func TestTCSM117_T1IReloadRefusesAnUnknown(t *testing.T) {
	h := newSMHost(t, "tcsm117")
	good := smConfig(h.pub, "")
	install := func(cfg string) {
		t.Helper()
		src := filepath.Join(t.TempDir(), "nexusd.yaml")
		if err := os.WriteFile(src, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		lib(t, "install-config", src, nexusdConfig)
	}
	// Step 1: an unknown key at the end of the file.
	install(good + "bogusKey: 1\n")
	wantLine := strings.Count(good, "\n") + 1
	r := h.cli("nexusd", "reload", "-o", "json")
	p := r.problem()
	errs, _ := p["errors"].([]any)
	if r.code != 1 || smSlug(p) != string(problem.ConfigInvalid) || len(errs) == 0 {
		t.Fatalf("step 1: exit %d, problem %v; want exit 1 config-invalid with errors", r.code, p)
	}
	e0, _ := errs[0].(map[string]any)
	if got, want := fmt.Sprintf("%v:%v:%v", e0["file"], e0["line"], e0["column"]),
		fmt.Sprintf("%s:%d:1", nexusdConfig, wantLine); got != want {
		t.Errorf("step 1: errors[0] at %s, want %s (bogusKey): %v", got, want, e0)
	}
	if ops := h.nexusdOps(); len(ops) != 0 {
		t.Errorf("step 1: nexusd operations exist: %+v", ops)
	}
	if pid := unitPID(t); pid != h.pid0 {
		t.Errorf("step 1: MainPID %d, want %d", pid, h.pid0)
	}
	// Step 2: restored, with limits.maxBodyBytes changed.
	install(smConfig(h.pub, "limits: {maxBodyBytes: 2097152}\n"))
	r = h.cli("nexusd", "reload", "-o", "json")
	if r.code != 0 || r.opID() == "" {
		t.Fatalf("step 2: exit %d, want 0", r.code)
	}
	op := h.mustOp(r.opID())
	res := decodeResult[spec.ReloadResult](t, op)
	if op.Status != apitypes.StatusSucceeded || !res.Applied || !slices.Equal(res.RestartRequired, []string{"limits"}) {
		t.Errorf("step 2: %s %s, want Succeeded {applied true, restartRequired [limits]}", op.Status, op.Result)
	}
	if pid := unitPID(t); pid != h.pid0 {
		t.Errorf("step 2: MainPID %d, want %d", pid, h.pid0)
	}
}

// TC-SM-118 (WI 7.2; FR-11, FR-9; W-10): T1 (j): a concurrent request
// during an update gets 409, including right after the restart.
func TestTCSM118_T1JAConcurrentRequestDuring(t *testing.T) {
	h := newSMHost(t, "tcsm118")
	stop := h.observe()
	r := h.cli("nexusd", "update", "--file", smTarball("v9.9.2"), "--no-wait", "-o", "json")
	U := r.opID()
	if r.code != 0 || U == "" {
		stop()
		t.Fatalf("update --no-wait: exit %d, want 0 with an operation", r.code)
	}
	type attempt struct {
		code            int
		status          any
		blocking        any
		pidBefore, pidA int
		uStatus         string
	}
	var attempts []attempt
	var created []string
	deadline := time.Now().Add(smOpMaxRuntime)
	for time.Now().Before(deadline) {
		u, err := h.op(U)
		if err == nil && terminal(u.Status) {
			break
		}
		uStatus := "unreachable"
		if err == nil {
			uStatus = string(u.Status)
		}
		a := attempt{pidBefore: unitPID(t), uStatus: uStatus}
		ar := h.cli("nexusd", "restart", "--no-wait", "-o", "json")
		a.pidA, a.code = unitPID(t), ar.code
		if p := ar.problem(); p != nil {
			a.status, a.blocking = p["status"], p["blockingOperationId"]
		}
		if id := ar.opID(); ar.code == 0 && id != "" {
			created = append(created, id)
		}
		attempts = append(attempts, a)
		time.Sleep(200 * time.Millisecond)
	}
	end := h.awaitOp(U, smOpMaxRuntime)
	requireNeverUnknown(t, stop())
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("update %s ended %s %v", U, end.Status, end.Error)
	}
	pidNew := unitPID(t)
	var conflicts, fromNew int
	for i, a := range attempts {
		t.Logf("attempt %d: U %s, MainPID %d -> %d, exit %d, status %v, blockingOperationId %v", i, a.uStatus, a.pidBefore,
			a.pidA, a.code, a.status, a.blocking)
		switch {
		case a.code == 1 && fmt.Sprint(a.status) == "409":
			if a.blocking != U {
				t.Errorf("attempt %d: 409 naming %v, want %s", i, a.blocking, U)
			}
			conflicts++
			if a.pidBefore == pidNew && a.pidA == pidNew && pidNew != h.pid0 {
				fromNew++
			}
		case a.code == cli5:
			// Not answered: nexusd was restarting.
		default:
			t.Errorf("attempt %d while %s was not terminal: exit %d, status %v; want 409 or no answer", i, U, a.code, a.status)
		}
	}
	t.Logf("%d attempts: %d answered 409 naming %s, %d of them by the new MainPID %d (was %d)", len(attempts), conflicts,
		U, fromNew, pidNew, h.pid0)
	if fromNew == 0 {
		t.Errorf("no 409 came from the new MainPID %d: the lane was not shown restored before the listener opened", pidNew)
	}
	for _, op := range h.nexusdOps() {
		if op.Action == spec.Restart.Op().ID {
			t.Errorf("a restart operation was created: %s (attempts that created one: %q)", op.ID, created)
		}
	}
}

// cli5 is the CLI's exit for a host that did not answer.
const cli5 = 5

// stampedLine is one output line with when it was read.
type stampedLine struct {
	at   time.Time
	text string
}

// TC-SM-119 (WI 7.2; FR-14): T1 (k): --follow prints log output after
// reconnecting across the restart.
func TestTCSM119_T1KFollowPrintsLogOutput(t *testing.T) {
	h := newSMHost(t, "tcsm119")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "nexusd", "update", "--file", smTarball("v9.9.2"), "--follow", "-o",
		"table")
	cmd.Env = pipelineEnv(h.home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []stampedLine
	var wg sync.WaitGroup
	read := func(name string, rd io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(rd)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, stampedLine{at: time.Now(), text: name + ": " + sc.Text()})
			mu.Unlock()
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	go read("stdout", outPipe)
	go read("stderr", errPipe)
	wg.Wait()
	err = cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&all, "%s %s\n", l.at.UTC().Format("15:04:05.000"), l.text)
	}
	t.Logf("nexus nexusd update --follow: exit %d\n%s", code, all.String())
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	reconnect, healthy := -1, -1
	seen := map[string]int{}
	stepLine := regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ \S.*$`)
	for i, l := range lines {
		if reconnect < 0 && strings.Contains(l.text, "likely restarting") {
			reconnect = i
		}
		if healthyRE.MatchString(l.text) {
			healthy = i
		}
		if m := stepLine.FindString(l.text); m != "" {
			seen[m]++
		}
	}
	if reconnect < 0 {
		t.Errorf("no output names \"likely restarting\"")
	}
	if healthy < 0 || healthy < reconnect {
		t.Errorf("no followed log line `healthy <ts>` after the reconnect (reconnect line %d, healthy line %d)", reconnect,
			healthy)
	}
	for l, n := range seen {
		if n > 1 {
			t.Errorf("followed line printed %d times: %q", n, l)
		}
	}
	if healthy >= 0 && !strings.HasPrefix(lines[healthy].text, "stdout: ") {
		t.Logf("note: the followed log lines are on stderr (FR-33: --follow streams the output to stderr)")
	}
}

// TC-SM-120 (WI 7.2; FR-7, ASM-6): T1: updating to the running version
// changes nothing.
func TestTCSM120_T1UpdatingToTheRunningVersion(t *testing.T) {
	h := newSMHost(t, "tcsm120")
	sum := sha(t, nexusdBin)
	u := h.update("v9.9.1")
	res := decodeResult[spec.UpdateResult](t, u.op)
	if u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded || res.Changed {
		t.Errorf("exit %d, %s %s; want exit 0 Succeeded {changed false}", u.run.code, u.op.Status, u.op.Result)
	}
	if u.pid1 != h.pid0 {
		t.Errorf("MainPID %d, want unchanged %d", u.pid1, h.pid0)
	}
	if s := sha(t, nexusdBin); s != sum {
		t.Errorf("%s changed: %s, was %s", nexusdBin, s, sum)
	}
	if _, err := os.Lstat(smPrev); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (%v)", smPrev, err)
	}
	requireStagingEmpty(t, "after finalize")
}

// TC-SM-121 (WI 7.2; FR-7, ASM-6): T1: a downgrade is refused without the
// flag and installed with it.
func TestTCSM121_T1ADowngradeIsRefusedWithout(t *testing.T) {
	h := newSMHost(t, "tcsm121")
	if u := h.update("v9.9.2"); u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded {
		t.Fatalf("setup update to v9.9.2: exit %d, %s", u.run.code, u.op.Status)
	}
	before := len(h.nexusdOps())
	r := h.cli("nexusd", "update", "--file", smTarball("v9.9.1"), "-o", "json")
	p := r.problem()
	if r.code != 1 || smSlug(p) != string(problem.VersionDowngrade) || p["runningVersion"] != "v9.9.2" ||
		p["packageVersion"] != "v9.9.1" {
		t.Errorf("step 1: exit %d, problem %v; want exit 1 version-downgrade v9.9.2 -> v9.9.1", r.code, p)
	}
	if es := stagingEntries(t); len(es) != 1 {
		t.Errorf("step 1: staging holds %q, want the refused upload still staged", es)
	}
	if n := len(h.nexusdOps()); n != before {
		t.Errorf("step 1: %d new operations", n-before)
	}
	u := h.update("v9.9.1", "--allow-downgrade")
	res := decodeResult[spec.UpdateResult](t, u.op)
	if u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded || res.PreviousVersion != "v9.9.2" ||
		res.NewVersion != "v9.9.1" {
		t.Errorf("step 2: exit %d, %s %s; want Succeeded {previousVersion v9.9.2, newVersion v9.9.1}", u.run.code,
			u.op.Status, u.op.Result)
	}
	if got := binVersion(t, nexusdBin); got != "nexusd v9.9.1" {
		t.Errorf("step 2: %s -version %q", nexusdBin, got)
	}
}

// TC-SM-122 (WI 7.2; FR-7, NFR-5): T1: a tarball with one byte flipped
// after signing is refused.
func TestTCSM122_T1ATarballWithOneByte(t *testing.T) {
	h := newSMHost(t, "tcsm122")
	b, err := os.ReadFile(smTarball("v9.9.2"))
	if err != nil {
		t.Fatal(err)
	}
	off := len(b) / 2
	b[off] ^= 0x01
	tampered := filepath.Join(t.TempDir(), filepath.Base(smTarball("v9.9.2")))
	if err := os.WriteFile(tampered, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha(t, nexusdBin)
	r := h.cli("nexusd", "update", "--file", tampered, "--signature", smTarball("v9.9.2")+".sig", "-o", "json")
	p := r.problem()
	t.Logf("flipped bit 0 of byte %d of %d", off, len(b))
	if r.code != 1 || smSlug(p) != string(problem.UpdateSignatureInvalid) {
		t.Errorf("exit %d, problem %v; want exit 1 update-signature-invalid", r.code, p)
	}
	if ops := h.nexusdOps(); len(ops) != 0 {
		t.Errorf("nexusd operations exist: %+v", ops)
	}
	requireStagingEmpty(t, "after the refusal")
	if s := sha(t, nexusdBin); s != sum {
		t.Errorf("%s changed", nexusdBin)
	}
}

// TC-SM-123 (WI 7.2; FR-8, FR-9, FR-18; A-6, A-44): T1 (l):
// update-rollback-failed is recorded by the next nexusd to start.
func TestTCSM123_T1UpdateRollbackFailedIsRecorded(t *testing.T) {
	h := newSMHost(t, "tcsm123")
	since := smWindow()
	r := h.cli("nexusd", "update", "--file", smTarball("v9.9.7"), "-o", "json")
	if r.code != 0 && r.code != 1 && r.code != cli5 {
		t.Errorf("step 1: exit %d, want 0, 1 or 5", r.code)
	}
	id := r.opID()
	if id == "" {
		es, err := os.ReadDir(smOpsDir)
		if err != nil || len(es) != 1 {
			t.Fatalf("step 1: no operation ID in the CLI output, and %s holds %v (%v)", smOpsDir, es, err)
		}
		id = es[0].Name()
	}
	unit := detach.Unit(id)
	// Step 2: wait for the op unit to end, polling the op meanwhile.
	var polls []string
	waitUntil(t, smOpMaxRuntime, unit+" to end", func() (bool, string) {
		if op, err := h.op(id); err == nil {
			polls = append(polls, string(op.Status)+":"+problemSlug(op))
		}
		st := showUnit(t, unit, "ActiveState")["ActiveState"]
		return st == "inactive" || st == "failed", st
	})
	before := showUnit(t, nexusdUnit, "ActiveState", "SubState", "Result", "MainPID", "NRestarts")
	prevV := binVersion(t, smPrev)
	t.Logf("before recovery: %s -version %q; nexusd.service %v", smPrev, prevV, before)
	if prevV != "nexusd v9.9.1" {
		t.Errorf("%s -version %q, want the clobbering stub's nexusd v9.9.1", smPrev, prevV)
	}
	if before["ActiveState"] == "active" {
		t.Errorf("nexusd.service is active before the manual recovery: the restore did not fail on systemd")
	}
	for _, f := range []string{detach.ExitFile, "result.json"} {
		if !exists(filepath.Join(smOpsDir, id, f)) {
			t.Errorf("%s does not exist before the recovery", filepath.Join(smOpsDir, id, f))
		}
	}
	j := unitJournal(t, nexusdUnit, since)
	if !strings.Contains(j, "nexusd stub v9.9.1: 'serve' fails on purpose") {
		t.Errorf("the journal lacks the restore stub's serve failures:\n%s", j)
	}
	t.Logf("nexusd.service journal since %s:\n%s", since, j)
	t.Logf("%s journal:\n%s", unit, mustSh(t, "journalctl", "-u", unit, "-o", "cat", "--no-pager"))

	// Step 3: recover by hand from the known-good v9.9.1 tree.
	t.Logf("lib.sh install-release: %s", lib(t, "install-release", smTarball("v9.9.1")))
	mustSh(t, detach.Systemctl, "reset-failed", nexusdUnit)
	mustSh(t, detach.Systemctl, "start", nexusdUnit)
	if got := binVersion(t, nexusdBin); got != "nexusd v9.9.1" {
		t.Errorf("after recovery: %s -version %q", nexusdBin, got)
	}
	requireActive(t, "after the manual recovery")

	// Step 4: the op as the new nexusd recorded it. It classifies the
	// recovered op after it is ready (FR-6), so poll until terminal.
	op, g := h.pollCLIOp(id, &polls)
	if g.code != 0 {
		t.Fatalf("operation get %s: exit %d %s", id, g.code, g.stdout)
	}
	t.Logf("polls between steps 2 and 4: %q", polls)
	requireNoUnknownPolls(t, polls)
	if op.Status != apitypes.StatusFailed || op.Error == nil || !strings.HasSuffix(op.Error.Type,
		string(problem.UpdateRollbackFailed)) || errExt(op, "rolledBack") != "false" {
		t.Fatalf("operation %s %v, want Failed update-rollback-failed rolledBack false", op.Status, op.Error)
	}
	for _, want := range []string{"may be down", smRecoveryAnchor} {
		if !strings.Contains(op.Error.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, op.Error.Detail)
		}
	}
	t.Logf("op log:\n%s", h.opLog(id))
}

// TC-SM-124 (WI 7.2; FR-12; A-31): T1: rollback is refused when the
// previous binary rejects the current config.
func TestTCSM124_T1RollbackIsRefusedWhenThe(t *testing.T) {
	h := newSMHost(t, "tcsm124")
	stub := "#!/bin/sh\ncase \"$1\" in\n-version) echo 'nexusd v9.9.0' ;;\n" +
		"validate-config) echo 'tc124 first error line' >&2; echo 'tc124 second error line' >&2; exit 1 ;;\n" +
		"*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(smPrev, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	binSum, prevSum := sha(t, nexusdBin), sha(t, smPrev)
	r := h.cli("nexusd", "rollback", "--yes", "-o", "json")
	p := r.problem()
	var msgs []string
	errs, _ := p["errors"].([]any)
	for _, e := range errs {
		if m, ok := e.(map[string]any); ok {
			msgs = append(msgs, fmt.Sprint(m["message"]))
		}
	}
	if r.code != 1 || smSlug(p) != string(problem.ConfigInvalid) ||
		!slices.Equal(msgs, []string{"tc124 first error line", "tc124 second error line"}) {
		t.Errorf("exit %d, problem %v (messages %q); want exit 1 config-invalid with the two stub lines in order", r.code,
			p, msgs)
	}
	if ops := h.nexusdOps(); len(ops) != 0 {
		t.Errorf("nexusd operations exist: %+v", ops)
	}
	if sha(t, nexusdBin) != binSum || sha(t, smPrev) != prevSum {
		t.Errorf("the binaries changed")
	}
}

// TC-SM-125 (WI 7.2; FR-2, FR-3, FR-1; A-31): T1: get and validate-config
// read back the real host state.
func TestTCSM125_T1GetAndValidateConfigRead(t *testing.T) {
	h := newSMHost(t, "tcsm125")
	get := func(step string) spec.DaemonProperties {
		t.Helper()
		r := h.cli("nexusd", "get", "-o", "json")
		var ds []spec.Daemon
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &ds) != nil || len(ds) != 1 {
			t.Fatalf("%s: exit %d %s", step, r.code, r.stdout)
		}
		return ds[0].Properties
	}
	d1 := get("step 1")
	t1 := time.Now()
	if d1.Version != "v9.9.1" || d1.Previous.Present || d1.ConfigPath != nexusdConfig || d1.ExecutablePath != nexusdBin ||
		!d1.SelfManagement.Available || !d1.UpdateConfigured {
		t.Errorf("step 1: %+v", d1)
	}
	if u := h.update("v9.9.2"); u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded {
		t.Fatalf("update to v9.9.2: exit %d, %s", u.run.code, u.op.Status)
	}
	d2 := get("step 2")
	elapsed := int64(time.Since(t1).Seconds()) + 1
	if d2.Version != "v9.9.2" || d2.Previous != (spec.Previous{Present: true, Path: smPrev, Version: "v9.9.1"}) ||
		d2.UptimeSeconds >= d1.UptimeSeconds+elapsed {
		t.Errorf("step 2: %+v (step 1 uptime %d s, %d s elapsed)", d2, d1.UptimeSeconds, elapsed)
	}
	r := h.cli("nexusd", "validate-config", "-o", "json")
	var vs []spec.ValidateResult
	if err := json.Unmarshal([]byte(r.stdout), &vs); err != nil {
		var v spec.ValidateResult
		if json.Unmarshal([]byte(r.stdout), &v) == nil {
			vs = []spec.ValidateResult{v}
		}
	}
	if r.code != 0 || len(vs) != 1 || !vs[0].Valid || vs[0].ConfigPath != nexusdConfig {
		t.Errorf("step 3: exit %d %s, want valid true", r.code, r.stdout)
	}
	readHome := h.cliHome(createToken(t, "tcsm125-read", spec.ReadScope))
	for _, args := range [][]string{
		{"nexusd", "update", "--file", smTarball("v9.9.2"), "-o", "json"},
		{"nexusd", "restart", "-o", "json"},
	} {
		r := h.cliAs(readHome, args...)
		if r.code != 3 || !strings.Contains(r.stderr, "403") && !strings.Contains(r.stderr, spec.AdminScope) {
			t.Errorf("step 4: %s with nexusd:read only: exit %d, stderr %s; want exit 3 naming %s", args[1], r.code,
				r.stderr, spec.AdminScope)
		}
		if !strings.Contains(r.stderr, spec.AdminScope) {
			t.Errorf("step 4: %s: stderr does not name %s", args[1], spec.AdminScope)
		}
	}
	if pid := unitPID(t); pid <= 0 {
		t.Errorf("nexusd.service has no MainPID")
	}
}

// p95 is the nearest-rank 95th percentile.
func p95(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[int(math.Ceil(0.95*float64(len(s))))-1]
}

// TC-SM-126 (WI 7.2; NFR-1): T1: update downtime p95 under 15 s, and
// rollback within healthTimeout + 20 s.
func TestTCSM126_T1NFR1UpdateDowntimeP95(t *testing.T) {
	var ups []time.Duration
	for i := range 5 {
		t.Run(fmt.Sprintf("update-%d", i+1), func(t *testing.T) {
			h := newSMHost(t, fmt.Sprintf("tcsm126u%d", i+1))
			u := h.update("v9.9.2")
			if u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded {
				t.Fatalf("exit %d, %s", u.run.code, u.op.Status)
			}
			first, _, healthy := nfr1Times(t, u.log)
			d := healthy.Sub(first)
			ups = append(ups, d)
			t.Logf("NFR NSM-NFR-1 update run %d: restart-started %s -> healthy %s = %s; CLI exit 0 in %s", i+1,
				first.Format(time.RFC3339Nano), healthy.Format(time.RFC3339Nano), d, u.run.took.Round(time.Millisecond))
		})
	}
	if len(ups) == 5 {
		p := p95(ups)
		t.Logf("NFR NSM-NFR-1 update restart-started -> healthy over 5 runs: %v; p95 (max of 5) %s", ups, p)
		if p >= 15*time.Second {
			t.Errorf("update p95 %s, want under 15 s", p)
		}
	} else {
		t.Errorf("%d of 5 update runs measured", len(ups))
	}
	for _, c := range []struct{ name, v string }{{"b", "v9.9.3"}, {"c", "v9.9.4"}, {"d", "v9.9.5"}} {
		t.Run("rollback-"+c.name, func(t *testing.T) {
			_, u := rollbackCase(t, "tcsm126"+c.name, c.v)
			first, last, healthy := nfr1Times(t, u.log)
			whole, restore := healthy.Sub(first), healthy.Sub(last)
			t.Logf("NFR NSM-NFR-1 rollback (%s) %s: first restart-started %s, restore restart-started %s, healthy %s; "+
				"first -> healthy %s, restore -> healthy %s; bound %s", c.name, c.v, first.Format(time.RFC3339Nano),
				last.Format(time.RFC3339Nano), healthy.Format(time.RFC3339Nano), whole, restore, smRollbackBound)
			if whole > smRollbackBound {
				t.Errorf("rollback case %s: %s from restart-started to the post-restore healthy, want at most %s", c.name,
					whole, smRollbackBound)
			}
		})
	}
}

// smPrevReal is where TC-SM-127 keeps the real previous binary while a
// counting wrapper stands in for nexusd.prev.
const smPrevReal = "/usr/local/bin/nexusd-tc127-real"

// TC-SM-127 (WI 7.2; NFR-2): T1: get and validate-config p95 under 200 ms,
// with nexusd.prev executed at most once.
func TestTCSM127_T1NFR2GetAndValidate(t *testing.T) {
	h := newSMHost(t, "tcsm127")
	if u := h.update("v9.9.2"); u.run.code != 0 || u.op.Status != apitypes.StatusSucceeded {
		t.Fatalf("update to v9.9.2: exit %d, %s", u.run.code, u.op.Status)
	}
	// A wrapper counts the execs of nexusd.prev: the sandboxed nexusd can
	// write only its state and logs directories, so the count goes there.
	count := filepath.Join(nexusdState, "tc127-prev-execs")
	mustSh(t, "mv", smPrev, smPrevReal)
	wrapper := fmt.Sprintf("#!/bin/sh\necho \"$*\" >>%s\nexec %s \"$@\"\n", count, smPrevReal)
	if err := os.WriteFile(smPrev, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := binVersion(t, smPrev); got != "nexusd v9.9.1" {
		t.Fatalf("the wrapper reports %q", got)
	}
	if err := os.Remove(count); err != nil {
		t.Fatal(err)
	}
	measure := func(name, method, path string) []time.Duration {
		t.Helper()
		var ds []time.Duration
		for range 50 {
			start := time.Now()
			code, b, err := h.rest(method, path, nil, "")
			d := time.Since(start)
			if err != nil || code != http.StatusOK {
				t.Fatalf("%s: %d %v %s", name, code, err, b)
			}
			ds = append(ds, d)
		}
		s := slices.Clone(ds)
		slices.Sort(s)
		t.Logf("NFR NSM-NFR-2 %s over 50 loopback calls: p50 %s, p95 %s, max %s", name, s[24], p95(ds), s[49])
		if p := p95(ds); p >= 200*time.Millisecond {
			t.Errorf("%s p95 %s, want under 200 ms", name, p)
		}
		return ds
	}
	measure("get (nexusd.daemons.list)", http.MethodGet, smDaemonsAt)
	measure("validate-config", http.MethodPost, smActionsAt+"validate-config")
	if v := h.daemon().Previous; v.Version != "v9.9.1" {
		t.Errorf("previous %+v through the wrapper, want v9.9.1", v)
	}
	b, err := os.ReadFile(count)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	execs := strings.Count(string(b), "\n")
	t.Logf("NFR NSM-NFR-2 nexusd.prev executed %d times over 101 reads: %q", execs, b)
	if execs > 1 {
		t.Errorf("nexusd.prev executed %d times, want at most once (cached by file identity)", execs)
	}
}

// vmRSS is VmRSS of pid in KiB.
func vmRSS(pid int) (int64, error) { return procStatusKiB(pid, "VmRSS") }

// procStatusKiB is a KiB field of /proc/<pid>/status, such as VmHWM.
func procStatusKiB(pid int, key string) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			f := strings.Fields(v)
			if len(f) == 0 {
				break
			}
			return strconv.ParseInt(f[0], 10, 64)
		}
	}
	return 0, fmt.Errorf("no %s for %d", key, pid)
}

// randomSignature is a well-formed signature that verifies nothing.
func randomSignature(t *testing.T) string {
	t.Helper()
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// upload POSTs the file at path as a release upload and returns the
// status and body.
func (h *smHost) upload(path, sig string) (int, []byte) {
	h.t.Helper()
	f, err := os.Open(path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+exactAddr+smUploadsAt+"?signature="+url.QueryEscape(sig), f)
	if err != nil {
		h.t.Fatal(err)
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("upload %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// TC-SM-128 (WI 7.2; NFR-4, FR-6): T1: upload RSS bound, and staging
// cleanup after a refusal and after an expiry sweep.
func TestTCSM128_T1NFR4UploadRSSBound(t *testing.T) {
	h := newSMHost(t, "tcsm128")
	big := filepath.Join(t.TempDir(), "random-64MiB.tar.gz")
	data := make([]byte, spec.DefaultMaxUploadBytes)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, data, 0o600); err != nil {
		t.Fatal(err)
	}
	data = nil
	// Step 1: idle RSS, then every 50 ms during the upload.
	pid := unitPID(t)
	for range 3 {
		h.daemon()
	}
	var idle []int64
	for range 20 {
		v, err := vmRSS(pid)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, v)
		time.Sleep(50 * time.Millisecond)
	}
	var base int64
	for _, v := range idle {
		base += v
	}
	base /= int64(len(idle))
	var mu sync.Mutex
	var samples []int64
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if v, err := vmRSS(pid); err == nil {
				mu.Lock()
				samples = append(samples, v)
				mu.Unlock()
			}
			select {
			case <-quit:
				return
			case <-tick.C:
			}
		}
	}()
	hwm0, err := procStatusKiB(pid, "VmHWM")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, b := h.upload(big, randomSignature(t))
	took := time.Since(start)
	close(quit)
	<-done
	hwm1, err := procStatusKiB(pid, "VmHWM")
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusCreated {
		t.Fatalf("64 MiB upload: %d %s", code, b)
	}
	var up spec.UploadResult
	if err := json.Unmarshal(b, &up); err != nil {
		t.Fatal(err)
	}
	// The 50 ms samples can miss a short peak; VmHWM, the process's
	// resident high-water mark, cannot. When the upload raised it, it is
	// the upload's peak.
	peak := slices.Max(samples)
	if hwm1 > hwm0 {
		peak = max(peak, hwm1)
	}
	delta := float64(peak-base) / 1024
	t.Logf("NFR NSM-NFR-4 64 MiB upload in %s: VmRSS idle %d KiB (mean of %d samples, %v), sampled peak %d KiB over %d "+
		"samples (%v), VmHWM %d -> %d KiB; peak - idle %.2f MiB; upload %s %d bytes", took.Round(time.Millisecond), base,
		len(idle), idle, slices.Max(samples), len(samples), samples, hwm0, hwm1, delta, up.UploadID, up.SizeBytes)
	if up.SizeBytes != spec.DefaultMaxUploadBytes {
		t.Errorf("stored %d bytes, want %d", up.SizeBytes, spec.DefaultMaxUploadBytes)
	}
	if delta >= 8 {
		t.Errorf("VmRSS grew %.2f MiB during the upload, want under 8 MiB", delta)
	}
	if unitPID(t) != pid {
		t.Errorf("nexusd restarted during the upload")
	}

	// Step 2: an update with that upload's bad signature is refused and
	// the upload is deleted.
	code, b, err = h.rest(http.MethodPost, smActionsAt+"update", strings.NewReader(`{"uploadId":"`+up.UploadID+`"}`),
		"application/json")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	if smSlug(p) != string(problem.UpdateSignatureInvalid) {
		t.Errorf("step 2: update %d %s, want update-signature-invalid", code, b)
	}
	requireStagingEmpty(t, "step 2, after the refused update")

	// Step 3: an expired upload is swept at the next start.
	small := filepath.Join(t.TempDir(), "small.tar.gz")
	if err := os.WriteFile(small, []byte("not a release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, b = h.upload(small, randomSignature(t))
	if code != http.StatusCreated || json.Unmarshal(b, &up) != nil {
		t.Fatalf("step 3: upload %d %s", code, b)
	}
	metaPath := filepath.Join(smStaging, up.UploadID, "meta.json")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	meta["expires"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if mb, err = json.Marshal(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, mb, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("step 3: staged %q, %s expires set to the past", stagingEntries(t), metaPath)
	mustSh(t, detach.Systemctl, "restart", nexusdUnit)
	requireActive(t, "step 3, after the restart")
	requireStagingEmpty(t, "step 3, after the restart's sweep")
}

// The F-SYS fake's encoded systemd behaviour (internal/providers/nexusd,
// fakesystem_test.go), which TC-SM-129 checks against real systemd. The
// values are systemd 259's, as captured in
// build/harness/logs/20261113T210341Z/test.log.
const (
	fsysStartBurst     = 5
	fsysNRestartsAtHit = 5           // The re-spawn refused at the limit is counted
	fsysResultAtLimit  = "exit-code" // The last start's failed Result is kept
	fsysLimitError     = "start request repeated too quickly"
	fsysNoBlockRefused = false // Restart (--no-block) returns nil; the start is refused later
	fsysLimitSticky    = false // The limit lifts once StartLimitIntervalSec has passed
)

const (
	fidelityUnit     = "nexusd-fidelity.service"
	fidelityUnitFile = "/etc/systemd/system/" + fidelityUnit
	fidelityBin      = "/usr/local/bin/nexusd-fidelity"
)

// fidelityShow is the exact `systemctl show` output the helper's Show
// parses, for the fidelity unit, and its properties.
func fidelityShow(t *testing.T) (string, map[string]string) {
	t.Helper()
	out := mustSh(t, detach.Systemctl, "show", fidelityUnit, "-p", "ActiveState", "-p", "SubState", "-p", "Result", "-p",
		"MainPID", "-p", "NRestarts")
	m := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			m[k] = v
		}
	}
	return out, m
}

// capture logs a systemctl show capture for testdata/systemctl-show.
func capture(t *testing.T, name, out string) {
	t.Helper()
	t.Logf("CAPTURE %s BEGIN\n%sCAPTURE %s END", name, out, name)
}

// TC-SM-129 (WI 7.2; NFR-6): T1: real systemd confirms the fake's encoded
// behaviours, on a copy of the shipped unit, nexusd-fidelity.service, that
// differs only in its ExecStart binary. It also captures the
// testdata/systemctl-show fixtures.
func TestTCSM129_T1RealSystemdConfirmsTheFake(t *testing.T) {
	smWipe(t)
	t.Cleanup(func() {
		smWipe(t)
		restoreHarness(t)
	})
	v1 := unpackFixture(t, "v9.9.1")
	installTree(t, v1)
	stubTree := unpackFixture(t, "v9.9.4")
	unit, err := os.ReadFile(shippedUnit)
	if err != nil {
		t.Fatal(err)
	}
	fid := strings.Replace(string(unit), "ExecStart="+nexusdBin+" ", "ExecStart="+fidelityBin+" ", 1)
	if fid == string(unit) {
		t.Fatalf("the shipped unit has no ExecStart=%s line", nexusdBin)
	}
	if err := os.WriteFile(fidelityUnitFile, []byte(fid), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sh(time.Minute, detach.Systemctl, "stop", fidelityUnit)
		_, _ = sh(time.Minute, detach.Systemctl, "reset-failed", fidelityUnit)
		for _, p := range []string{fidelityUnitFile, fidelityBin, fidelityBin + ".new"} {
			if err := os.RemoveAll(p); err != nil {
				t.Error(err)
			}
		}
		if out, err := sh(time.Minute, detach.Systemctl, "daemon-reload"); err != nil {
			t.Errorf("daemon-reload: %v %s", err, out)
		}
	})
	mustSh(t, detach.Systemctl, "daemon-reload")
	put := func(src string) {
		t.Helper()
		mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", src, fidelityBin+".new")
		mustSh(t, "mv", "-f", fidelityBin+".new", fidelityBin)
	}
	pidOf := func() int {
		t.Helper()
		_, m := fidelityShow(t)
		pid, err := strconv.Atoi(m["MainPID"])
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	t.Logf("real systemd: %s", strings.SplitN(mustSh(t, detach.Systemctl, "--version"), "\n", 2)[0])

	// Step 1: --no-block restart of a healthy unit returns at once; the new
	// MainPID appears later.
	put(filepath.Join(v1, "nexusd"))
	mustSh(t, detach.Systemctl, "start", fidelityUnit)
	pidA := pidOf()
	out, m := fidelityShow(t)
	if m["ActiveState"] != "active" || m["SubState"] != "running" {
		t.Fatalf("fidelity unit after start: %v", m)
	}
	capture(t, "active.txt", out)
	start := time.Now()
	rout, rerr := sh(time.Minute, detach.Systemctl, "restart", "--no-block", fidelityUnit)
	took := time.Since(start)
	_, right := fidelityShow(t)
	var pidB int
	waitUntil(t, 30*time.Second, "a new active MainPID", func() (bool, string) {
		_, m := fidelityShow(t)
		pidB, _ = strconv.Atoi(m["MainPID"])
		return m["ActiveState"] == "active" && pidB > 0 && pidB != pidA, fmt.Sprint(m)
	})
	t.Logf("step 1: F-SYS: Restart (--no-block) returns at once and the new PID comes later; real: returned in %s "+
		"(%v %q), right after %v, then MainPID %d -> %d", took, rerr, rout, right, pidA, pidB)
	if rerr != nil || took >= time.Second {
		t.Errorf("step 1: systemctl restart --no-block took %s (%v), want under 1 s and success", took, rerr)
	}

	// The activating capture: Type=notify before READY=1.
	hang := filepath.Join(t.TempDir(), "hang")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	put(hang)
	mustSh(t, detach.Systemctl, "restart", "--no-block", fidelityUnit)
	waitUntil(t, 30*time.Second, "activating before READY", func() (bool, string) {
		_, m := fidelityShow(t)
		return m["ActiveState"] == "activating" && m["SubState"] == "start" && m["MainPID"] != "0", fmt.Sprint(m)
	})
	out, _ = fidelityShow(t)
	capture(t, "activating.txt", out)
	mustSh(t, detach.Systemctl, "stop", fidelityUnit)

	// Step 2: a blocking restart of the stub that exits at start; the start
	// limit is hit within 10 s. A poller catches the auto-restart state.
	put(filepath.Join(stubTree, "nexusd"))
	// The stopped unit may already be unloaded, and reset-failed then exits
	// 1 ("not loaded"): there is no state to clear either way.
	if out, err := sh(time.Minute, detach.Systemctl, "reset-failed", fidelityUnit); err != nil {
		t.Logf("reset-failed of the stopped unit: %v %s", err, strings.TrimSpace(out))
	}
	var autoRestart string
	pollQuit, pollDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-pollQuit:
				return
			default:
			}
			o, err := sh(10*time.Second, detach.Systemctl, "show", fidelityUnit, "-p", "ActiveState", "-p", "SubState",
				"-p", "Result", "-p", "MainPID", "-p", "NRestarts")
			if err == nil && strings.Contains(o, "SubState=auto-restart\n") && autoRestart == "" {
				autoRestart = o
			}
		}
	}()
	t2 := time.Now()
	bout, berr := sh(time.Minute, detach.Systemctl, "restart", fidelityUnit)
	t.Logf("step 2: blocking restart of the v9.9.4 stub: %v\n%s", berr, bout)
	if berr == nil {
		t.Errorf("step 2: the blocking restart of a binary that exits at start succeeded")
	}
	var hit map[string]string
	waitUntil(t, 10*time.Second, "the unit to fail for good", func() (bool, string) {
		_, hit = fidelityShow(t)
		return hit["ActiveState"] == "failed", fmt.Sprint(hit)
	})
	hitAfter := time.Since(t2)
	close(pollQuit)
	<-pollDone
	out, hit = fidelityShow(t)
	capture(t, "failed-after-restarts.txt", out)
	if autoRestart != "" {
		capture(t, "auto-restart.txt", autoRestart)
	} else {
		t.Logf("the poller did not catch SubState=auto-restart within RestartSec")
	}
	j2 := unitJournal(t, fidelityUnit, "@"+strconv.FormatInt(t2.Add(-time.Second).Unix(), 10))
	t.Logf("step 2: F-SYS: failed starts end failed with Result=%s, NRestarts %d (burst %d); real: %v after %s, "+
		"%d Starting lines; journal:\n%s", fsysResultAtLimit, fsysNRestartsAtHit, fsysStartBurst, hit,
		hitAfter.Round(time.Millisecond), strings.Count(j2, "Starting "), j2)
	if hit["Result"] != fsysResultAtLimit || hit["NRestarts"] != strconv.Itoa(fsysNRestartsAtHit) {
		t.Errorf("step 2: real systemd ends failed with Result=%s NRestarts=%s, F-SYS encodes Result=%s NRestarts=%d",
			hit["Result"], hit["NRestarts"], fsysResultAtLimit, fsysNRestartsAtHit)
	}

	// Step 3: while the limit holds, a blocking restart fails, and a
	// --no-block restart returns 0 but its start is refused: the unit stays
	// failed.
	s3 := time.Now()
	bout, berr = sh(time.Minute, detach.Systemctl, "restart", fidelityUnit)
	out, afterBlock := fidelityShow(t)
	capture(t, "failed-start-limit.txt", out)
	nout, nerr := sh(time.Minute, detach.Systemctl, "restart", "--no-block", fidelityUnit)
	time.Sleep(500 * time.Millisecond)
	_, afterNoBlock := fidelityShow(t)
	j3 := unitJournal(t, fidelityUnit, "@"+strconv.FormatInt(s3.Add(-time.Second).Unix(), 10))
	t.Logf("step 3: F-SYS: the start is refused with %q while the limit holds, Restart (--no-block) returns an "+
		"error: %v; real: blocking restart %v %q, then %v; --no-block restart %v %q, then %v; journal:\n%s",
		fsysLimitError, fsysNoBlockRefused, berr, strings.TrimSpace(bout), afterBlock, nerr, strings.TrimSpace(nout),
		afterNoBlock, j3)
	if berr == nil || !(strings.Contains(strings.ToLower(bout+j3), fsysLimitError) ||
		strings.Contains(bout, "attempted too often")) {
		t.Errorf("step 3: the restart was not refused for the start limit: %v %q", berr, bout)
	}
	if (nerr != nil) != fsysNoBlockRefused {
		t.Errorf("step 3: real systemctl restart --no-block at the start limit: %v %q; F-SYS refuses it: %v", nerr,
			strings.TrimSpace(nout), fsysNoBlockRefused)
	}
	if afterNoBlock["ActiveState"] != "failed" || afterNoBlock["MainPID"] != "0" ||
		afterNoBlock["Result"] != fsysResultAtLimit {
		t.Errorf("step 3: after the --no-block restart at the start limit the unit is %v, F-SYS encodes failed, "+
			"MainPID 0, Result=%s", afterNoBlock, fsysResultAtLimit)
	}

	// The limit's lifetime: once StartLimitIntervalSec has passed since the
	// window began, is a start accepted without reset-failed?
	time.Sleep(time.Until(t2.Add(11 * time.Second)))
	lout, lerr := sh(time.Minute, detach.Systemctl, "start", fidelityUnit)
	_, afterLife := fidelityShow(t)
	jl := unitJournal(t, fidelityUnit, "@"+strconv.FormatInt(time.Now().Add(-2*time.Second).Unix(), 10))
	lifted := strings.Contains(jl, "'serve' fails on purpose")
	t.Logf("lifetime: F-SYS keeps the limit until ResetFailed: %v; real: a start %s after the window began: %v %q, then "+
		"%v (the binary ran again without reset-failed: %v)", fsysLimitSticky, time.Since(t2).Round(time.Millisecond),
		lerr, strings.TrimSpace(lout), afterLife, lifted)
	if lifted == fsysLimitSticky {
		t.Errorf("lifetime: real systemd ran the binary again %s after the window began without reset-failed: %v; "+
			"F-SYS keeps the limit until ResetFailed: %v", time.Since(t2).Round(time.Second), lifted, fsysLimitSticky)
	}

	// Step 4: reset-failed, then the restart is accepted; the healthy
	// binary starts, as F7's restore does.
	waitUntil(t, 15*time.Second, "the unit to settle", func() (bool, string) {
		_, m := fidelityShow(t)
		return m["ActiveState"] == "failed" || m["ActiveState"] == "inactive", fmt.Sprint(m)
	})
	mustSh(t, detach.Systemctl, "reset-failed", fidelityUnit)
	_, reset := fidelityShow(t)
	put(filepath.Join(v1, "nexusd"))
	rout, rerr = sh(time.Minute, detach.Systemctl, "restart", fidelityUnit)
	_, m = fidelityShow(t)
	t.Logf("step 4: F-SYS: ResetFailed clears the limit; real: after reset-failed %v; restart %v %q; then %v", reset,
		rerr, strings.TrimSpace(rout), m)
	if rerr != nil || m["ActiveState"] != "active" {
		t.Errorf("step 4: restart after reset-failed: %v %q, unit %v", rerr, rout, m)
	}
	mustSh(t, detach.Systemctl, "stop", fidelityUnit)

	// Step 5: install.sh with nexusd.service inactive starts nothing.
	if st := activeState(t, nexusdUnit); st == "active" {
		t.Fatalf("nexusd.service is %s before step 5", st)
	}
	since := smWindow()
	iout := installTree(t, unpackFixture(t, "v9.9.2"))
	started := startedLines(t, since)
	t.Logf("step 5: F-SYS: install.sh restarts only an active service; real: %d Started lines %q, nexusd.service %s",
		len(started), started, activeState(t, nexusdUnit))
	if len(started) != 0 || strings.Contains(iout, "restarted the running") {
		t.Errorf("step 5: install.sh on an inactive service started it: %q", started)
	}
}
