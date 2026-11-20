// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-43 to TC-RA-46
// and TC-RA-E10 (WI 2.4): the generated nexusdtoken commands against real
// servers running the real nexusd provider, each behind a proxy that counts
// its requests.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/provider"
	nexusdprov "github.com/mauktik/nexus/internal/providers/nexusd"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// tokenHostDaemon is the provider.Daemon handed to the real nexusd
// provider: only its state directory matters to the token ops.
type tokenHostDaemon struct{ stateDir string }

func (d tokenHostDaemon) ConfigPath() string                          { return filepath.Join(d.stateDir, "nexusd.yaml") }
func (d tokenHostDaemon) StateDir() string                            { return d.stateDir }
func (d tokenHostDaemon) StartedAt() time.Time                        { return time.Time{} }
func (d tokenHostDaemon) CheckConfig() provider.ConfigCheck           { return provider.ConfigCheck{} }
func (d tokenHostDaemon) CheckConfigData([]byte) provider.ConfigCheck { return provider.ConfigCheck{} }
func (d tokenHostDaemon) Reload(context.Context) (provider.ReloadResult, error) {
	return provider.ReloadResult{}, nil
}

// tokenHost is a real nexus server over the real nexusd provider, reached
// through a proxy that counts the requests the CLI sends.
type tokenHost struct {
	*providerServer
	proxy string // The URL the client configuration names
	mu    sync.Mutex
	posts int
	authz []string // The Authorization header of each request
}

// startTokenHost serves the real nexusd provider as own. Its "cli" token
// holds nexusd:admin and audit:read.
func startTokenHost(t *testing.T, own string) *tokenHost {
	t.Helper()
	p := nexusdprov.New(nexusdprov.Options{Host: own})
	h := &tokenHost{providerServer: startProviderServer(t, own, p, true, nexusdspec.AdminScope, "audit:read")}
	p.UseDaemon(tokenHostDaemon{stateDir: h.stateDir})
	target, err := url.Parse(h.url)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if r.Method == http.MethodPost {
			h.posts++
		}
		h.authz = append(h.authz, r.Header.Get("Authorization"))
		h.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(ps.Close)
	h.proxy = ps.URL
	return h
}

// takeAuthz returns the Authorization headers seen so far, and resets them.
func (h *tokenHost) takeAuthz() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.authz
	h.authz = nil
	return out
}

func (h *tokenHost) postCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posts
}

// mint creates a token in the host's store.
func (h *tokenHost) mint(t *testing.T, name string, scopes ...string) (string, auth.Token) {
	t.Helper()
	sec, tok, err := auth.Create(h.stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return sec, tok
}

// get GETs path on the host directly with token and returns the status
// and body.
func (h *tokenHost) get(t *testing.T, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 10 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (h *tokenHost) tokens(t *testing.T) []auth.Token {
	t.Helper()
	l, err := auth.List(h.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// tokenEnv is the environment of a CLI run under home, with no terminal.
func tokenEnv(home string) Env {
	env := homeEnv(home)
	env.OpenTerminal = noTerminal
	return env
}

// TC-RA-43: nexusdtoken add prints one distinct secret per host on stdout, labelled, after confirmation.
func TestTCRA43_NexusdtokenAddPrintsOneDistinctSecretPerHostOn(t *testing.T) {
	a, b := startTokenHost(t, "srv-a"), startTokenHost(t, "srv-b")
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil}, clientHost{"b", b.proxy, b.token, nil})

	// Step 1: no terminal and no --yes.
	r := runCLI(tokenEnv(home), "nexusdtoken", "add", "ci", "--host", "a,b")
	if r.code != ExitUsage {
		t.Errorf("without --yes: exit %d, want %d: %s", r.code, ExitUsage, r.stderr)
	}
	if !strings.Contains(r.stderr, "--yes") {
		t.Errorf("without --yes: stderr %q does not name --yes", r.stderr)
	}
	if a.postCount() != 0 || b.postCount() != 0 {
		t.Fatalf("without --yes: POSTs a=%d b=%d, want 0", a.postCount(), b.postCount())
	}

	// Step 2: with --yes.
	r = runCLI(tokenEnv(home), "nexusdtoken", "add", "ci", "--host", "a,b", "--yes")
	if r.code != ExitOK {
		t.Fatalf("with --yes: exit %d: %s", r.code, r.stderr)
	}
	secrets := map[string]string{}
	for _, l := range jsonLines(t, "stdout", r.stdout) {
		h, _ := l["host"].(string)
		tok, _ := l["token"].(string)
		if _, dup := secrets[h]; dup || tok == "" {
			t.Fatalf("stdout line %v: a second result for %q, or no token", l, h)
		}
		secrets[h] = tok
	}
	if len(secrets) != 2 || secrets["a"] == "" || secrets["b"] == "" || secrets["a"] == secrets["b"] {
		t.Fatalf("stdout results %q, want one distinct token for each of a and b:\n%s", secrets, r.stdout)
	}
	if a.postCount() != 1 || b.postCount() != 1 {
		t.Errorf("with --yes: POSTs a=%d b=%d, want 1 each", a.postCount(), b.postCount())
	}

	// Each token authenticates only on its own host.
	for _, c := range []struct {
		name      string
		own, peer *tokenHost
	}{{"a", a, b}, {"b", b, a}} {
		if st, body := c.own.get(t, "/v1/capabilities", secrets[c.name]); st != http.StatusOK {
			t.Errorf("%s's token on %s: %d %s", c.name, c.name, st, body)
		}
		if st, _ := c.peer.get(t, "/v1/capabilities", secrets[c.name]); st != http.StatusUnauthorized {
			t.Errorf("%s's token on the other host: %d, want 401", c.name, st)
		}
	}

	// No leak on stderr.
	for h, s := range secrets {
		if n := strings.Count(r.stderr, s); n != 0 {
			t.Errorf("stderr has %d occurrences of %s's secret", n, h)
		}
	}
}

// TC-RA-44: nexusdtoken remove of the caller's own token warns on stderr and exits 0.
func TestTCRA44_NexusdtokenRemoveOfTheCallerSOwnTokenWarnsOn(t *testing.T) {
	a := startTokenHost(t, "srv-a")
	me, meTok := a.mint(t, "me", nexusdspec.AdminScope)
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, me, nil})

	// Step 1: destructive, so refused without a terminal or --yes.
	r := runCLI(tokenEnv(home), "nexusdtoken", "remove", "me")
	if r.code != ExitUsage || a.postCount() != 0 {
		t.Fatalf("without --yes: exit %d, %d POSTs, want %d and 0: %s", r.code, a.postCount(), ExitUsage, r.stderr)
	}

	// Step 2.
	r = runCLI(tokenEnv(home), "nexusdtoken", "remove", "me", "--yes")
	if r.code != ExitOK {
		t.Fatalf("with --yes: exit %d: %s", r.code, r.stderr)
	}
	lines := jsonLines(t, "stdout", r.stdout)
	if len(lines) != 1 || lines[0]["revokedSelf"] != true || lines[0]["tokenId"] != meTok.ID {
		t.Errorf("stdout %q, want one result with revokedSelf true for %s", r.stdout, meTok.ID)
	}
	want := fmt.Sprintf("a: the token this request used (me, id %s) is revoked: this client can no longer "+
		"authenticate to this host with it", meTok.ID)
	if got := warningLines(t, r.stderr, true); !slices.Equal(got, []string{want}) {
		t.Errorf("warning lines %q, want [%q] (stderr %q)", got, want, r.stderr)
	}
	r = runCLI(tokenEnv(home), "nexusdtoken", "remove", "-o", "table", "me", "--yes")
	if r.code == ExitOK {
		t.Errorf("a second remove with the revoked token exited 0")
	}

	// Step 3: the client's token no longer authenticates.
	r = runCLI(tokenEnv(home), "nexusdtoken", "get")
	if r.code != ExitAuth {
		t.Errorf("get after revoking itself: exit %d, want %d: %s", r.code, ExitAuth, r.stderr)
	}
}

// TC-RA-44, the table format: the warning is a text line through a.warn.
func TestTCRA44_RemoveSelfWarnsInTextOnATable(t *testing.T) {
	a := startTokenHost(t, "srv-a")
	me, meTok := a.mint(t, "me", nexusdspec.AdminScope)
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, me, nil})
	r := runCLI(tokenEnv(home), "nexusdtoken", "remove", "me", "--yes", "-o", "table")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := fmt.Sprintf("nexus: warning: host a: the token this request used (me, id %s) is revoked: this client "+
		"can no longer authenticate to this host with it", meTok.ID)
	if got := warningLines(t, r.stderr, false); !slices.Equal(got, []string{want}) {
		t.Errorf("warning lines %q, want [%q] (stderr %q)", got, want, r.stderr)
	}
}

// TC-RA-45: nexusdtoken get prints the token table with the self column.
func TestTCRA45_NexusdtokenGetPrintsTheTokenTableWithTheSelf(t *testing.T) {
	a := startTokenHost(t, "srv-a")
	a.mint(t, "t2", nexusdspec.ReadScope)
	a.mint(t, "t3")
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil})
	stored := a.tokens(t)
	if len(stored) != 3 {
		t.Fatalf("%d stored tokens, want 3", len(stored))
	}

	// Step 1: the table.
	r := runCLI(tokenEnv(home), "nexusdtoken", "get", "-o", "table")
	if r.code != ExitOK {
		t.Fatalf("get -o table: exit %d: %s", r.code, r.stderr)
	}
	// A table always leads with the client-side host name.
	wantHeader := []string{"HOST"}
	for _, c := range nexusdspec.TokensList.Op().Columns {
		wantHeader = append(wantHeader, strings.ToUpper(strings.TrimPrefix(c, "properties.")))
	}
	rows := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if header := strings.Fields(rows[0]); !slices.Equal(header, wantHeader) {
		t.Errorf("header %q, want %q", header, wantHeader)
	}
	if len(rows) != 1+len(stored) {
		t.Fatalf("table %q: %d rows, want %d", r.stdout, len(rows)-1, len(stored))
	}
	for _, tk := range stored {
		i := slices.IndexFunc(rows[1:], func(row string) bool { return strings.Fields(row)[1] == tk.Name })
		if i < 0 {
			t.Errorf("no row for %s", tk.Name)
			continue
		}
		fields := strings.Fields(rows[1+i])
		wantSelf := fmt.Sprint(tk.Name == "cli")
		if fields[0] != "a" || fields[2] != tk.ID || fields[len(fields)-1] != wantSelf {
			t.Errorf("row %q: want tokenId %s and self %s", rows[1+i], tk.ID, wantSelf)
		}
	}

	// Step 2: JSON.
	j := runCLI(tokenEnv(home), "nexusdtoken", "get", "-o", "json")
	if j.code != ExitOK {
		t.Fatalf("get -o json: exit %d: %s", j.code, j.stderr)
	}
	var items []nexusdspec.Token
	if err := json.Unmarshal([]byte(j.stdout), &items); err != nil {
		t.Fatalf("json stdout %q: %v", j.stdout, err)
	}
	if len(items) != len(stored) {
		t.Fatalf("%d JSON items, want %d", len(items), len(stored))
	}
	for _, tk := range stored {
		i := slices.IndexFunc(items, func(it nexusdspec.Token) bool { return it.Name == tk.Name })
		if i < 0 {
			t.Errorf("no JSON item for %s", tk.Name)
			continue
		}
		p := items[i].Properties
		if p.TokenID != tk.ID || !slices.Equal(p.Scopes, tk.Scopes) || !p.Created.Equal(tk.Created) ||
			(p.Expires == nil) != (tk.Expires == nil) || p.Self != (tk.Name == "cli") {
			t.Errorf("item %s: %+v, want auth.List's %+v with self %v", tk.Name, p, tk, tk.Name == "cli")
		}
	}

	for _, out := range []string{r.stdout, j.stdout} {
		for _, bad := range []string{"sha256", "nxs_"} {
			if strings.Contains(out, bad) {
				t.Errorf("output holds %q:\n%s", bad, out)
			}
		}
	}
}

// TC-RA-46: --save-as-host stores the new token and the next command uses it.
func TestTCRA46_SaveAsHostStoresTheNewTokenAndTheNextCommandUses(t *testing.T) {
	a := startTokenHost(t, "srv-a")
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil})

	// Step 1.
	r := runCLI(tokenEnv(home), "nexusdtoken", "add", "ci2", "--scopes", nexusdspec.ReadScope, "--host", "a",
		"--save-as-host", "a-ci2")
	if r.code != ExitOK {
		t.Fatalf("add --save-as-host: exit %d: %s", r.code, r.stderr)
	}
	path, err := clientconfig.Path(homeEnv(home).Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := cfg.Hosts["a-ci2"]
	if !ok || !strings.HasPrefix(saved.Token, "nxs_") || saved.URL != a.proxy {
		t.Fatalf("saved host a-ci2 %+v (present %v), want the proxy URL and an nxs_ token", saved, ok)
	}
	lines := jsonLines(t, "stdout", r.stdout)
	if len(lines) != 1 || lines[0]["name"] != "ci2" {
		t.Fatalf("stdout %q, want one ci2 result", r.stdout)
	}
	if _, has := lines[0]["token"]; has {
		t.Errorf("stdout result has a token member: %v", lines[0])
	}
	for _, out := range []struct{ what, s string }{{"stdout", r.stdout}, {"stderr", r.stderr}} {
		if n := strings.Count(out.s, saved.Token); n != 0 {
			t.Errorf("%s has %d occurrences of the secret", out.what, n)
		}
	}

	// Step 2: the saved host's token is the one used.
	a.takeAuthz()
	r = runCLI(tokenEnv(home), "nexusd", "get", "--host", "a-ci2")
	if r.code != ExitOK {
		t.Fatalf("nexusd get --host a-ci2: exit %d: %s", r.code, r.stderr)
	}
	if got := a.takeAuthz(); len(got) == 0 || slices.ContainsFunc(got, func(h string) bool { return h != "Bearer "+saved.Token }) {
		t.Errorf("nexusd get --host a-ci2 sent Authorization %d times, not always with the saved token", len(got))
	}
	// nexusd get is an unaudited read, and ci2's nexusd:read reaches no
	// audited op: the server's audit names ci2 through the denial of one
	// that needs nexusd:admin.
	r = runCLI(tokenEnv(home), "nexusdtoken", "get", "--host", "a-ci2")
	if r.code != ExitAuth {
		t.Errorf("nexusdtoken get --host a-ci2: exit %d, want %d: %s", r.code, ExitAuth, r.stderr)
	}
	q := url.Values{"filter": {"tokenName=ci2"}}
	st, body := a.get(t, auditListPath(t)+"?"+q.Encode(), a.token)
	if st != http.StatusOK {
		t.Fatalf("audit query: %d %s", st, body)
	}
	var recs struct {
		Items []systemspec.AuditRecord `json:"items"`
	}
	if err := json.Unmarshal(body, &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs.Items) == 0 {
		t.Fatalf("no audit record with the actor ci2: %s", body)
	}
	for _, rec := range recs.Items {
		if rec.TokenName != "ci2" || rec.Action != nexusdspec.TokensList.Op().ID || rec.Outcome != "denied" {
			t.Errorf("audit record %+v, want the denial of ci2 on nexusd.tokens.list", rec)
		}
	}
}

// auditListPath is the REST path of system.audit.list.
func auditListPath(t *testing.T) string {
	t.Helper()
	for _, op := range catalog.Ops() {
		if op.ID == systemspec.AuditList.Op().ID {
			return "/v1" + op.Path
		}
	}
	t.Fatal("system.audit.list is not in the catalog")
	return ""
}

// TC-RA-E10: CLI token commands accept no token value as an argument.
func TestTCRAE10_CLITokenCommandsAcceptNoTokenValueAsAnArgument(t *testing.T) {
	// Step 1: every flag and positional of add, get and remove.
	a := testApp(catalog.All())
	for _, verb := range []string{"add", "get", "remove"} {
		cmd := tcraCommand(t, a, "nexusdtoken", verb)
		var names []string
		cmd.Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
		cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
		if len(names) == 0 {
			t.Fatalf("nexusdtoken %s: no flags listed", verb)
		}
		for _, n := range names {
			if slices.Contains([]string{"token", "secret", "value"}, n) {
				t.Errorf("nexusdtoken %s has a --%s flag", verb, n)
			}
		}
		if verb == "remove" && !strings.HasPrefix(cmd.Use, "remove NAME") {
			t.Errorf("nexusdtoken remove: use %q, want the positional NAME", cmd.Use)
		}
	}

	t.Run("a token string as NAME", func(t *testing.T) {
		// A full nxs_<id>_<secret> matches tokenNamePattern (60 characters
		// of [A-Za-z0-9_-]); the name schema's not-token-shape refine
		// refuses it before it reaches a handler, a log or the audit target.
		tokenStringAsName(t)
	})
}

// tokenStringAsName is TC-RA-E10's second half: a full token string given
// as NAME is refused by the name pattern with 400 validation-failed, and
// is echoed neither in the problem, on stderr, nor in the audit log.
func tokenStringAsName(t *testing.T) {
	h := startTokenHost(t, "srv-a")
	home := writeClientConfig(t, "a", clientHost{"a", h.proxy, h.token, nil})
	// SEC-1: the variants of a live token's secret, too.
	sec, minted := h.mint(t, "minted")
	secret := strings.SplitN(sec, "_", 3)[2]
	variants := []string{h.token, sec, minted.ID + "_" + secret, "NXS_" + minted.ID + "_" + secret, secret,
		"ci-" + secret, secret + "-x"}
	// Secrets that start with "-" and "_" also fail the name's own pattern:
	// fixed, so the client is always shown the refusal, not the pattern.
	// NAME follows "--": a leading "-" is otherwise parsed (and echoed by
	// cobra's own flag error) as a flag, not read as the name.
	for _, fixed := range []string{"-" + strings.Repeat("aB3_", 10) + "xy", "_" + strings.Repeat("-Z9a", 10) + "xy"} {
		variants = append(variants, fixed, "ci-"+fixed, fixed+"-x", minted.ID+"_"+fixed, "NXS_"+minted.ID+"_"+fixed)
	}
	before := h.tokens(t)
	for _, name := range variants {
		for _, verb := range []string{"remove", "add"} {
			for _, f := range []string{"json", "table"} {
				r := runCLI(tokenEnv(home), "nexusdtoken", verb, "--yes", "-o", f, "--", name)
				if r.code != ExitUsage {
					t.Errorf("-o %s: %s of a token string: exit %d, want %d (validation-failed): %s", f, verb, r.code, ExitUsage, r.stderr)
				}
				// The CLI checks the same input schema before sending, so the
				// refusal is its local usage error; the REST call below gets
				// the server's 400 validation-failed. It shows the schema's
				// own message, not the validator's "not: validated against".
				if !strings.Contains(r.stderr, "looks like, or contains, a token string") ||
					strings.Contains(r.stderr, "validated against") {
					t.Errorf("-o %s: %s: stderr %q lacks the refusal's message", f, verb, r.stderr)
				}
				for _, secret := range []string{h.token, sec, secret, name} {
					if strings.Contains(r.stderr, secret) || strings.Contains(r.stdout, secret) {
						t.Errorf("-o %s: %s: the token string is echoed: stdout %q stderr %q", f, verb, r.stdout, r.stderr)
					}
				}
			}
		}
	}
	// The not-found and conflict problems do not echo the supplied name.
	if r := runCLI(tokenEnv(home), "nexusdtoken", "remove", "ghost-probe", "--yes"); r.code == 0 ||
		strings.Contains(r.stderr, "ghost-probe") || strings.Contains(r.stdout, "ghost-probe") ||
		!strings.Contains(r.stderr, "no token has this name or ID") {
		t.Errorf("remove of an unknown name: exit %d stdout %q stderr %q, want a failure with the neutral not-found", r.code, r.stdout, r.stderr)
	}
	if r := runCLI(tokenEnv(home), "nexusdtoken", "add", "minted", "--yes"); r.code == 0 ||
		strings.Contains(r.stderr, `"minted"`) || !strings.Contains(r.stderr, minted.ID) {
		t.Errorf("duplicate add: exit %d stderr %q, want a failure naming only the existing ID", r.code, r.stderr)
	}
	method, path := http.MethodPost, "/v1/providers/nexusd/tokens/"+h.token+"/remove"
	req, err := http.NewRequest(method, h.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	c := &http.Client{Timeout: 10 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "validation-failed") ||
		strings.Contains(string(body), h.token) {
		t.Errorf("REST remove of a token string: %d %s, want 400 validation-failed without the string", resp.StatusCode, body)
	}
	if after := h.tokens(t); len(after) != len(before) {
		t.Errorf("tokens %d -> %d", len(before), len(after))
	}
	q := url.Values{"filter": {"action=" + nexusdspec.TokensRemove.Op().ID}}
	st, recs := h.get(t, auditListPath(t)+"?"+q.Encode(), h.token)
	if st != http.StatusOK {
		t.Fatalf("audit query: %d %s", st, recs)
	}
	for _, secret := range append([]string{h.token, sec, secret}, variants...) {
		if strings.Contains(string(recs), secret) {
			t.Errorf("the audit log holds the token string or a variant: %s", recs)
		}
	}
}
