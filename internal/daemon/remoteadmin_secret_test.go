// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-37 and
// TC-RA-38 (WI 2.4): the remote token ops on a real daemon.New with the
// built-in nexusd provider, checked for the secret in every sink (NFR-1)
// and for any operation trace (CON-3).

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// raDaemon is a real daemon.New over known state, audit and log sinks, with
// providers.nexusd enabled and the built-in providers.
type raDaemon struct {
	base     string
	stateDir string
	auditDir string
	logs     *syncBuffer
	admin    string // nexusd:admin, nexusd:read and audit:read
	client   *http.Client
}

func startRADaemon(t *testing.T) *raDaemon {
	t.Helper()
	r := &raDaemon{stateDir: t.TempDir(), auditDir: t.TempDir(), logs: &syncBuffer{},
		client: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(r.client.CloseIdleConnections)
	listen := freePort(t)
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeConfig(t, cfgPath, listen, "nexusd")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := auth.Create(r.stateDir, auth.CreateRequest{Name: "admin",
		Scopes: []string{nexusdspec.AdminScope, nexusdspec.ReadScope, "audit:read"}})
	if err != nil {
		t.Fatal(err)
	}
	r.admin = admin
	log := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := New(cfg, Options{ConfigPath: cfgPath, Logger: log, StateDir: r.stateDir, AuditDir: r.auditDir,
		Detach: detachtest.Local(), Signals: make(chan os.Signal)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(ShutdownTimeout + 5*time.Second):
			t.Error("Run still running after the test")
		}
	})
	select {
	case <-d.Ready():
	case err := <-runErr:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready within 10s")
	}
	r.base = "http://" + listen
	return r
}

// raResp is one HTTP answer.
type raResp struct {
	status int
	header http.Header
	body   []byte
}

func (r *raDaemon) do(t *testing.T, method, path, token string, header map[string]string) raResp {
	t.Helper()
	req, err := http.NewRequest(method, r.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raResp{status: resp.StatusCode, header: resp.Header, body: b}
}

func (r *raDaemon) add(t *testing.T, name string, header map[string]string) raResp {
	t.Helper()
	return r.do(t, http.MethodPost, "/v1/providers/nexusd/tokens/"+name+"/add", r.admin, header)
}

// want fails the test unless the answer has status.
func (a raResp) want(t *testing.T, status int, what string) raResp {
	t.Helper()
	if a.status != status {
		t.Fatalf("%s: %d %.400s, want %d", what, a.status, a.body, status)
	}
	return a
}

func (a raResp) slug(t *testing.T) problem.Slug {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(a.body, &p); err != nil {
		t.Fatalf("not a problem: %v: %.300s", err, a.body)
	}
	return p.Slug()
}

// cliRun runs the nexus CLI in this process against the daemon with the
// admin token, and no terminal.
func (r *raDaemon) cliRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Getenv: func(k string) string {
			switch k {
			case clientconfig.EnvHost:
				return r.base
			case clientconfig.EnvToken:
				return r.admin
			case "HOME":
				return t.TempDir()
			}
			return ""
		},
		OpenTerminal: func() (cli.Terminal, error) { return nil, os.ErrNotExist },
	}
	code = cli.Run(args, env)
	return code, out.String(), errb.String()
}

// raStoreHashes maps each stored token's name to its sha256 hex, read from
// the raw tokens.json.
func raStoreHashes(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tokens []struct {
			Name string `json:"name"`
			Hash string `json:"sha256"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("tokens.json: %v", err)
	}
	out := map[string]string{}
	for _, tk := range f.Tokens {
		out[tk.Name] = tk.Hash
	}
	return out
}

// filesUnder returns the contents of every regular file under dir whose
// base name matches glob, keyed by path.
func filesUnder(t *testing.T, dir, glob string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return out
	}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		if ok, _ := filepath.Match(glob, e.Name()); !ok {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pages GETs every page of a list path and returns each body.
func (r *raDaemon) pages(t *testing.T, path string) [][]byte {
	t.Helper()
	var out [][]byte
	cursor := ""
	for range 1000 {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		a := r.do(t, http.MethodGet, path+sep+q.Encode(), r.admin, nil).want(t, http.StatusOK, path)
		out = append(out, a.body)
		var page struct {
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(a.body, &page); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if page.NextCursor == "" {
			return out
		}
		cursor = page.NextCursor
	}
	t.Fatalf("%s: more than 1000 pages", path)
	return nil
}

// TC-RA-37: REST and CLI token creates leave the secret in no sink of a real daemon.
func TestTCRA37_RESTAndCLITokenCreatesLeaveTheSecretInNoSinkOfA(t *testing.T) {
	d := startRADaemon(t)
	var problems [][]byte

	// Step 1: a REST add, keeping the body.
	step1 := d.add(t, "rest1", nil).want(t, http.StatusOK, "REST add rest1")
	var rest1 nexusdspec.TokenAddResult
	if err := json.Unmarshal(step1.body, &rest1); err != nil {
		t.Fatal(err)
	}

	// Step 2: the CLI add.
	code, stdout, stderr := d.cliRun(t, "nexusdtoken", "add", "cli1", "--yes", "-o", "json")
	if code != cli.ExitOK {
		t.Fatalf("nexus nexusdtoken add cli1: exit %d: %s", code, stderr)
	}
	var printed []nexusdspec.TokenAddResult
	if err := json.Unmarshal([]byte(stdout), &printed); err != nil || len(printed) != 1 {
		t.Fatalf("CLI stdout %q: %v", stdout, err)
	}
	cli1 := printed[0]

	// Step 3: a duplicate add gives 409 with no secret.
	before := raStoreHashes(t, d.stateDir)
	dup := d.add(t, "rest1", nil).want(t, http.StatusConflict, "duplicate add of rest1")
	if s := dup.slug(t); s != problem.Conflict {
		t.Errorf("duplicate add: problem %s, want %s", s, problem.Conflict)
	}
	problems = append(problems, dup.body)

	// Step 4: a replay with an Idempotency-Key is refused and adds nothing.
	replay := d.add(t, "rest2", map[string]string{"Idempotency-Key": "k-1"}).
		want(t, http.StatusBadRequest, "add with an Idempotency-Key")
	if s := replay.slug(t); s != problem.ValidationFailed {
		t.Errorf("keyed add: problem %s, want %s", s, problem.ValidationFailed)
	}
	problems = append(problems, replay.body)
	replay1 := d.add(t, "rest1", map[string]string{"Idempotency-Key": "k-2"}).
		want(t, http.StatusBadRequest, "replay of step 1 with an Idempotency-Key")
	problems = append(problems, replay1.body)
	if after := raStoreHashes(t, d.stateDir); !mapsEqual(before, after) {
		t.Errorf("tokens.json changed by the refused adds: %v -> %v", before, after)
	}

	// Non-vacuity: each secret is where it should be, and both hashes are
	// in tokens.json.
	hashes := raStoreHashes(t, d.stateDir)
	type secret struct{ name, token, part, hash string }
	var secrets []secret
	for _, r := range []nexusdspec.TokenAddResult{rest1, cli1} {
		parts := strings.SplitN(r.Token, "_", 3)
		if len(parts) != 3 || parts[0] != "nxs" || parts[1] != r.TokenID || len(parts[2]) != 43 {
			t.Fatalf("%s: token %q is not nxs_<tokenId>_<secret>", r.Name, r.Token)
		}
		h := hashes[r.Name]
		if len(h) != 64 {
			t.Fatalf("tokens.json has no sha256 for %s: %v", r.Name, hashes)
		}
		secrets = append(secrets, secret{r.Name, r.Token, parts[2], h})
	}
	if !bytes.Contains(step1.body, []byte(secrets[0].token)) || !strings.Contains(stdout, secrets[1].token) {
		t.Fatal("non-vacuity: step 1's body or the CLI stdout lacks its secret")
	}

	// Both tokens authenticate on the next request.
	for _, s := range secrets {
		d.do(t, http.MethodGet, "/v1/capabilities", s.token, nil).want(t, http.StatusOK, s.name+" authenticates")
	}

	// A failing CLI call: a duplicate-name add. A successful --yes add prints
	// nothing on stderr, so only a failure gives that sink anything to hold.
	dupCode, dupOut, dupErr := d.cliRun(t, "nexusdtoken", "add", "rest1", "--yes")
	if dupCode == cli.ExitOK || strings.TrimSpace(dupErr) == "" {
		t.Fatalf("non-vacuity: the CLI's duplicate add: exit %d, stdout %q, stderr %q, want a failure with a message",
			dupCode, dupOut, dupErr)
	}

	// Step 5: collect the sinks: every regular file in the state directory
	// but tokens.json, which holds the hashes by design and is checked on its
	// own below.
	sinks := map[string][]byte{}
	var sawDB bool
	for p, b := range filesUnder(t, d.stateDir, "*") {
		switch base := filepath.Base(p); {
		case base == auth.TokenFileName:
			continue
		case strings.HasPrefix(base, store.FileName):
			sawDB = true
		}
		sinks[p] = b
	}
	if !sawDB {
		t.Fatal("no state.db in the state directory")
	}
	audits := filesUnder(t, d.auditDir, "audit.jsonl*")
	if len(audits) == 0 {
		t.Fatal("no audit.jsonl in the audit directory")
	}
	for p, b := range audits {
		sinks[p] = b
	}
	sinks["daemon log"] = []byte(logText(d.logs))
	sinks["CLI stderr (add)"] = []byte(stderr)
	sinks["CLI stderr (duplicate add)"] = []byte(dupErr)
	sinks["CLI stdout (duplicate add)"] = []byte(dupOut)
	for i, b := range problems {
		sinks["problem body "+strconv.Itoa(i)] = b
	}
	for i, b := range d.pages(t, "/v1/operations") {
		sinks["GET /v1/operations page "+strconv.Itoa(i)] = b
	}
	auditPages := d.pages(t, auditPath(t))
	for i, b := range auditPages {
		sinks["GET audit page "+strconv.Itoa(i)] = b
	}
	// The audit API and file hold the adds' records, so the scan reads them.
	if !bytes.Contains(bytes.Join(auditPages, nil), []byte(`"nexusd.tokens.add"`)) {
		t.Fatal("non-vacuity: the audit API has no nexusd.tokens.add record")
	}
	// The refused keyed adds of step 4 are logged with their op, so the log
	// scan reads a token-op line and not only startup chatter.
	if !strings.Contains(logText(d.logs), `"op":"`+nexusdspec.TokensAdd.Op().ID+`"`) {
		t.Fatal("non-vacuity: the daemon log has no line for nexusd.tokens.add")
	}

	// tokens.json holds each hash and neither the token nor its secret part.
	tokensJSON, err := os.ReadFile(filepath.Join(d.stateDir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		for _, needle := range []struct{ what, v string }{{"token", s.token}, {"secret part", s.part}} {
			if bytes.Contains(tokensJSON, []byte(needle.v)) {
				t.Errorf("tokens.json holds %s's %s", s.name, needle.what)
			}
		}
		if !bytes.Contains(tokensJSON, []byte(s.hash)) {
			t.Errorf("non-vacuity: tokens.json lacks %s's hash", s.name)
		}
	}

	for _, s := range secrets {
		for _, needle := range []struct{ what, v string }{{"token", s.token}, {"secret part", s.part}, {"hash", s.hash}} {
			for name, b := range sinks {
				if n := bytes.Count(b, []byte(needle.v)); n != 0 {
					t.Errorf("%s: %d occurrences of %s's %s", name, n, s.name, needle.what)
				}
			}
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// auditPath is the REST path of system.audit.list.
func auditPath(t *testing.T) string {
	t.Helper()
	_, path, err := openapi.Route(systemspec.AuditList.Op())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TC-RA-38: Token ops leave no operation record and no operation-endpoint trace.
func TestTCRA38_TokenOpsLeaveNoOperationRecordAndNoOperation(t *testing.T) {
	d := startRADaemon(t)
	opsDir := filepath.Join(d.stateDir, "ops")
	entries := func() []string {
		es, err := os.ReadDir(opsDir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}
	countOps := func() []apitypes.Operation {
		var all []apitypes.Operation
		for _, b := range d.pages(t, "/v1/operations") {
			var page apitypes.List[apitypes.Operation]
			if err := json.Unmarshal(b, &page); err != nil {
				t.Fatal(err)
			}
			all = append(all, page.Items...)
		}
		return all
	}

	// Step 1: the operation count and the ops directory before.
	opsBefore, dirBefore := countOps(), entries()

	// Step 2: add, list and remove a token.
	d.add(t, "tc38", nil).want(t, http.StatusOK, "add tc38")
	d.do(t, http.MethodGet, "/v1/providers/nexusd/tokens", d.admin, nil).want(t, http.StatusOK, "list")
	d.do(t, http.MethodPost, "/v1/providers/nexusd/tokens/tc38/remove", d.admin, nil).want(t, http.StatusOK, "remove tc38")

	// Step 3: no new operation, none under nexusd.tokens.*.
	opsAfter := countOps()
	if len(opsAfter) != len(opsBefore) {
		t.Errorf("operation count %d -> %d", len(opsBefore), len(opsAfter))
	}
	for _, b := range d.pages(t, "/v1/operations?filter=provider%3Dnexusd") {
		var page apitypes.List[apitypes.Operation]
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		for _, o := range append(page.Items, opsAfter...) {
			if strings.HasPrefix(o.Action, "nexusd.tokens.") {
				t.Errorf("operation %s has action %s", o.ID, o.Action)
			}
		}
	}

	// Step 4: the add's requested record ID names no operation.
	var recs apitypes.List[systemspec.AuditRecord]
	q := url.Values{"filter": {"action=nexusd.tokens.add", "outcome=requested"}}
	a := d.do(t, http.MethodGet, auditPath(t)+"?"+q.Encode(), d.admin, nil).want(t, http.StatusOK, "audit query")
	if err := json.Unmarshal(a.body, &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs.Items) != 1 {
		t.Fatalf("%d requested records of the add, want 1: %s", len(recs.Items), a.body)
	}
	for _, r := range recs.Items {
		if r.OperationID != "" {
			t.Errorf("the add's requested record names operation %s", r.OperationID)
		}
	}
	got := d.do(t, http.MethodGet, "/v1/operations/"+recs.Items[0].ID, d.admin, nil)
	if got.status != http.StatusNotFound || got.slug(t) != problem.NotFound {
		t.Errorf("GET /v1/operations/{requested record ID}: %d %.300s, want 404 not-found", got.status, got.body)
	}

	// No new directory under ops/.
	if dirAfter := entries(); !slices.Equal(dirAfter, dirBefore) {
		t.Errorf("ops/ entries %q -> %q", dirBefore, dirAfter)
	}
}
