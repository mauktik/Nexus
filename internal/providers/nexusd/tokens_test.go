// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-30 to TC-RA-36,
// TC-RA-E8, TC-RA-E9 and TC-RA-ERR12 (WI 2.3): the remote token ops on the
// T0 stack, with the provider's clock on F-CLK.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// tokenStack is a T0 stack whose provider reads the virtual clock clk,
// which starts at t0 (F-CLK).
type tokenStack struct {
	*stack
	clk *fakeClock
	t0  time.Time
}

// newTokenStack starts the stack. t0 is whole seconds, so the RFC 3339
// facts (no fraction) equal the times, and near real time, so the
// authenticator, which reads the real clock, honors the expiries.
func newTokenStack(t *testing.T) *tokenStack {
	t.Helper()
	t0 := time.Now().UTC().Truncate(time.Second)
	clk := newFakeClock(t0)
	// fakeClock.Advance wants a waiter; the guard sets no timer, so hold one.
	clk.AfterFunc(100*365*24*time.Hour, func() {})
	s := newStack(t, stackConfig{opts: Options{Now: clk.Now}})
	return &tokenStack{stack: s, clk: clk, t0: t0}
}

// add POSTs nexusd.tokens.add for name with the optional JSON body.
func (s *tokenStack) add(t *testing.T, token, name, body string) response {
	t.Helper()
	return s.do(t, http.MethodPost, "/providers/nexusd/tokens/"+name+"/add", token, body, nil)
}

// added adds name as admin, wants 200 and returns the result.
func (s *tokenStack) added(t *testing.T, name, body string) spec.TokenAddResult {
	t.Helper()
	return decodeJSON[spec.TokenAddResult](t, s.add(t, s.admin, name, body).want(t, http.StatusOK).body)
}

// remove POSTs nexusd.tokens.remove for ref, a name or an ID.
func (s *tokenStack) remove(t *testing.T, token, ref string) response {
	t.Helper()
	return s.do(t, http.MethodPost, "/providers/nexusd/tokens/"+ref+"/remove", token, "", nil)
}

// list GETs nexusd.tokens.list and returns the raw body.
func (s *tokenStack) list(t *testing.T, token string) []byte {
	t.Helper()
	return s.do(t, http.MethodGet, "/providers/nexusd/tokens", token, "", nil).want(t, http.StatusOK).body
}

// advanceTo moves the virtual clock to at.
func (s *tokenStack) advanceTo(t *testing.T, at time.Time) {
	t.Helper()
	s.clk.Advance(t, at.Sub(s.clk.Now()))
}

// storeSum is the SHA-256 of tokens.json.
func (s *tokenStack) storeSum(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.stateDir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// storeHashes maps each stored token's name to its sha256 hex, read from
// the raw tokens.json.
func (s *tokenStack) storeHashes(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.stateDir, auth.TokenFileName))
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
	for _, r := range f.Tokens {
		out[r.Name] = r.Hash
	}
	return out
}

// tokens is auth.List of the stack's state directory.
func (s *tokenStack) tokens(t *testing.T) []auth.Token {
	t.Helper()
	l, err := auth.List(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// tokenNamed is the stored token named name, or nil.
func (s *tokenStack) tokenNamed(t *testing.T, name string) *auth.Token {
	t.Helper()
	for _, tk := range s.tokens(t) {
		if tk.Name == name {
			return &tk
		}
	}
	return nil
}

// idOf is the ID of the stored token named name.
func (s *tokenStack) idOf(t *testing.T, name string) string {
	t.Helper()
	tk := s.tokenNamed(t, name)
	if tk == nil {
		t.Fatalf("no stored token %q", name)
	}
	return tk.ID
}

// completedWith is the completed record of action whose facts have
// tokenName name.
func (s *tokenStack) completedWith(t *testing.T, action, name string) systemspec.AuditRecord {
	t.Helper()
	for _, r := range s.auditOf(t, action) {
		if r.Outcome == audit.OutcomeCompleted && r.Facts["tokenName"] == name {
			return r
		}
	}
	t.Fatalf("no completed %s record for %q: %v", action, name, outcomes(s.auditOf(t, action)))
	return systemspec.AuditRecord{}
}

// wantFacts fails unless facts holds each key=value of want.
func wantFacts(t *testing.T, facts map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got, ok := facts[k]; !ok || got != v {
			t.Errorf("fact %s = %q (present %v), want %q; facts %v", k, got, ok, v, facts)
		}
	}
}

// noSecretIn fails when b holds "nxs_", "sha256" or any of the given
// secrets or hashes.
func noSecretIn(t *testing.T, what string, b []byte, secrets ...string) {
	t.Helper()
	for _, s := range append([]string{"nxs_", "sha256"}, secrets...) {
		if s != "" && strings.Contains(string(b), s) {
			t.Errorf("%s holds %q: %s", what, s, b)
		}
	}
}

// wantTokenProblem fails unless r is a problem of slug at status, and returns it.
func wantTokenProblem(t *testing.T, r response, status int, slug problem.Slug) map[string]any {
	t.Helper()
	r.want(t, status)
	m := problemOf(t, r.body)
	if got := slugOf(m); got != string(slug) {
		t.Fatalf("slug %s, want %s: %s", got, slug, r.body)
	}
	return m
}

var tokenKeys = []string{"sha256", "hash", "secret", "token"}

// walkNoSecrets fails when v, walked as decoded JSON, has a key named as a
// secret or a string value that is a token.
func walkNoSecrets(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if slices.Contains(tokenKeys, k) {
				t.Errorf("%s has key %q", path, k)
			}
			walkNoSecrets(t, path+"/"+k, e)
		}
	case []any:
		for i, e := range x {
			walkNoSecrets(t, fmt.Sprintf("%s/%d", path, i), e)
		}
	case string:
		if strings.HasPrefix(x, "nxs_") {
			t.Errorf("%s is a token", path)
		}
	}
}

func TestTCRA30_TokenListMatchesAuthListMarksTheCallerSOwnToken(t *testing.T) {
	s := newTokenStack(t)
	if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: "ci", TTL: 30 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	adminID := s.idOf(t, "admin")
	if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: "bot",
		CreatedBy: &auth.TokenRef{ID: adminID, Name: "admin"}}); err != nil {
		t.Fatal(err)
	}

	// Step 1.
	body := s.list(t, s.admin)
	got := decodeJSON[apitypes.List[spec.Token]](t, body)
	// Step 2.
	want := s.tokens(t)
	if len(got.Items) != len(want) {
		t.Fatalf("items %d, want %d: %s", len(got.Items), len(want), body)
	}
	for i, w := range want {
		g := got.Items[i]
		if g.Name != w.Name || g.Properties.TokenID != w.ID || !slices.Equal(g.Properties.Scopes, w.Scopes) ||
			!g.Properties.Created.Equal(w.Created) {
			t.Errorf("item %d = %+v, want %+v", i, g, w)
		}
		if (g.Properties.Expires == nil) != (w.Expires == nil) ||
			(w.Expires != nil && !g.Properties.Expires.Equal(*w.Expires)) {
			t.Errorf("item %s expires %v, want %v", w.Name, g.Properties.Expires, w.Expires)
		}
		if g.ID != apitypes.ResourceID(spec.Name, spec.TokensType, w.Name) || g.Type != spec.TokensType ||
			g.Host != StackHost {
			t.Errorf("item %s resource = %s %s %s", w.Name, g.ID, g.Type, g.Host)
		}
		if g.Properties.Self != (w.Name == "admin") {
			t.Errorf("item %s self = %v", w.Name, g.Properties.Self)
		}
		wantBy := (*spec.TokenRef)(nil)
		if w.Name == "bot" {
			wantBy = &spec.TokenRef{TokenID: adminID, Name: "admin"}
		}
		if (g.Properties.CreatedBy == nil) != (wantBy == nil) ||
			(wantBy != nil && *g.Properties.CreatedBy != *wantBy) {
			t.Errorf("item %s createdBy = %+v, want %+v", w.Name, g.Properties.CreatedBy, wantBy)
		}
	}
	raw := decodeJSON[map[string]any](t, body)
	walkNoSecrets(t, "", raw)
	for _, it := range raw["items"].([]any) {
		m := it.(map[string]any)
		props := m["properties"].(map[string]any)
		e, ok := props["expires"]
		if !ok {
			t.Errorf("%s: no expires member", m["name"])
		}
		if (m["name"] == "ci") == (e == nil) {
			t.Errorf("%s: expires = %v", m["name"], e)
		}
		if _, has := props["createdBy"]; has != (m["name"] == "bot") {
			t.Errorf("%s: createdBy present = %v", m["name"], has)
		}
	}

	// Steps 3 and 4.
	if _, err := auth.Revoke(s.stateDir, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range decodeJSON[apitypes.List[spec.Token]](t, s.list(t, s.admin)).Items {
		names = append(names, it.Name)
	}
	if slices.Contains(names, "ci") || !slices.Contains(names, "new") {
		t.Errorf("second list names %v: want new and not ci", names)
	}
}

func TestTCRA31_TokenAddReturnsTheSecretOnceWithMetadataAndThe(t *testing.T) {
	s := newTokenStack(t)
	ttl, err := auth.ParseLifetime("90d")
	if err != nil {
		t.Fatal(err)
	}

	// Step 1.
	res := s.added(t, "ci", `{"name":"ci","scopes":["nexusd:read"],"expires":"90d"}`)
	if !res.Changed {
		t.Error("changed false")
	}
	if !regexp.MustCompile(`^nxs_`+regexp.QuoteMeta(res.TokenID)+`_`).MatchString(res.Token) ||
		!regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(res.TokenID) {
		t.Errorf("token %q does not carry tokenId %q", res.Token, res.TokenID)
	}
	if res.Name != "ci" || !slices.Equal(res.Scopes, []string{spec.ReadScope}) {
		t.Errorf("name %q scopes %v", res.Name, res.Scopes)
	}
	if !res.Created.Equal(s.t0) {
		t.Errorf("created %v, want t0 %v", res.Created, s.t0)
	}
	if res.Expires == nil || !res.Expires.Equal(s.t0.Add(ttl)) {
		t.Errorf("expires %v, want %v", res.Expires, s.t0.Add(ttl))
	}

	// Step 2.
	s.getDaemon(t, res.Token)

	// Step 3.
	rec := s.tokenNamed(t, "ci")
	if rec == nil {
		t.Fatal("ci is not stored")
	}
	if rec.ID != res.TokenID || rec.CreatedBy == nil ||
		*rec.CreatedBy != (auth.TokenRef{ID: s.idOf(t, "admin"), Name: "admin"}) {
		t.Errorf("record %+v createdBy %+v", rec, rec.CreatedBy)
	}
}

func TestTCRA32_TokenRemoveByNameOrIDDeletesTheRecordAndTheToken(t *testing.T) {
	s := newTokenStack(t)
	a := s.added(t, "a", `{"scopes":["nexusd:read"]}`)
	b := s.added(t, "b", `{"scopes":["nexusd:read"]}`)

	for _, c := range []struct {
		ref string
		res spec.TokenAddResult
	}{{"a", a}, {b.TokenID, b}} {
		got := decodeJSON[spec.TokenRemoveResult](t, s.remove(t, s.admin, c.ref).want(t, http.StatusOK).body)
		if !got.Changed || got.TokenID != c.res.TokenID || got.Name != c.res.Name || got.RevokedSelf ||
			len(got.Warnings) != 0 {
			t.Errorf("remove %s = %+v", c.ref, got)
		}
	}
	for _, tok := range []string{a.Token, b.Token} {
		s.do(t, http.MethodGet, "/providers/nexusd/daemons", tok, "", nil).want(t, http.StatusUnauthorized)
	}
	if s.tokenNamed(t, "a") != nil || s.tokenNamed(t, "b") != nil {
		t.Errorf("auth.List still holds a or b: %+v", s.tokens(t))
	}
}

func TestTCRA33_RevokingTheCallerSOwnTokenAnswers200With(t *testing.T) {
	s := newTokenStack(t)
	me := s.mint("me", spec.AdminScope)
	id := s.idOf(t, "me")

	// Step 1.
	got := decodeJSON[spec.TokenRemoveResult](t, s.remove(t, me, "me").want(t, http.StatusOK).body)
	if !got.Changed || !got.RevokedSelf || got.TokenID != id || got.Name != "me" {
		t.Errorf("result %+v", got)
	}
	want := []string{"the token this request used (me, id " + id +
		") is revoked: this client can no longer authenticate to this host with it"}
	if !slices.Equal(got.Warnings, want) {
		t.Errorf("warnings %q, want %q", got.Warnings, want)
	}
	rec := s.completedWith(t, spec.TokensRemove.Op().ID, "me")
	wantFacts(t, rec.Facts, map[string]string{"tokenId": id, "tokenName": "me", "revokedSelf": "true"})

	// Step 2.
	s.do(t, http.MethodGet, "/providers/nexusd/daemons", me, "", nil).want(t, http.StatusUnauthorized)
}

func TestTCRA34_TenSuccessfulMutationsPerRollingMinuteThen429(t *testing.T) {
	for _, uncounted := range []bool{true, false} {
		t.Run(fmt.Sprintf("uncounted=%v", uncounted), func(t *testing.T) {
			s := newTokenStack(t)
			// Step 1: 10 successes 2 s apart, with the uncounted calls
			// planted between them.
			type step struct {
				run    func() response
				status int
			}
			addOK := func(n string) step {
				return step{func() response { return s.add(t, s.admin, n, "") }, http.StatusOK}
			}
			removeOK := func(n string) step {
				return step{func() response { return s.remove(t, s.admin, n) }, http.StatusOK}
			}
			plants := map[int]step{
				1: {func() response {
					return s.do(t, http.MethodGet, "/providers/nexusd/tokens", s.admin, "", nil)
				}, http.StatusOK},
				3: {func() response { return s.add(t, s.admin, "r1", "") }, http.StatusConflict},
				5: {func() response { return s.add(t, s.admin, "bad", `{"expires":"0s"}`) }, http.StatusBadRequest},
				7: {func() response { return s.remove(t, s.admin, "nope") }, http.StatusNotFound},
			}
			var seq []step
			for i := range 6 {
				seq = append(seq, addOK(fmt.Sprintf("r%d", i)))
			}
			for i := range 4 {
				seq = append(seq, removeOK(fmt.Sprintf("r%d", i)))
			}
			for i, st := range seq {
				st.run().want(t, st.status)
				if p, ok := plants[i]; ok && uncounted {
					p.run().want(t, p.status)
				}
				s.clk.Advance(t, 2*time.Second)
			}

			// Step 2.
			sum := s.storeSum(t)

			// Step 3: now is t0+20s and the oldest success is at t0.
			r := s.add(t, s.admin, "r6", "")
			m := wantTokenProblem(t, r, http.StatusTooManyRequests, problem.RateLimited)
			wantRetry := int64(s.t0.Add(time.Minute).Sub(s.clk.Now()) / time.Second)
			if got := r.header.Get("Retry-After"); got != strconv.FormatInt(wantRetry, 10) || wantRetry < 1 {
				t.Errorf("Retry-After %q, want %d", got, wantRetry)
			}
			if _, ok := m["detail"]; !ok {
				t.Errorf("problem without detail: %s", r.body)
			}
			if got := s.storeSum(t); got != sum {
				t.Error("the refused add changed tokens.json")
			}
			outs := outcomes(s.auditOf(t, spec.TokensAdd.Op().ID))
			if len(outs) < 2 || !slices.Equal(outs[len(outs)-2:], []string{"requested", "refused:rate-limited"}) {
				t.Errorf("add records end %v, want requested, refused:rate-limited", outs)
			}

			// Steps 4 and 5: the oldest success is exactly 60 s old.
			s.advanceTo(t, s.t0.Add(time.Minute))
			r = s.add(t, s.admin, "r6", "")
			wantTokenProblem(t, r, http.StatusTooManyRequests, problem.RateLimited)
			if got := r.header.Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After at the window edge %q, want 1", got)
			}

			// Step 6.
			s.clk.Advance(t, time.Nanosecond)
			s.add(t, s.admin, "r6", "").want(t, http.StatusOK)

			// Step 7: a rolling window freed exactly one slot, the t0 one. The
			// next oldest success is at t0+2s, so the next add is refused again,
			// asked to wait 2 s; a window that restarted at t0 or cleared every
			// success once the oldest expired would admit it.
			r = s.add(t, s.admin, "r7", "")
			wantTokenProblem(t, r, http.StatusTooManyRequests, problem.RateLimited)
			if got := r.header.Get("Retry-After"); got != "2" {
				t.Errorf("Retry-After after one slot freed %q, want 2", got)
			}
			s.advanceTo(t, s.t0.Add(62*time.Second))
			r = s.add(t, s.admin, "r7", "")
			wantTokenProblem(t, r, http.StatusTooManyRequests, problem.RateLimited)
			if got := r.header.Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After at the second slot's edge %q, want 1", got)
			}
			s.clk.Advance(t, time.Nanosecond)
			s.add(t, s.admin, "r7", "").want(t, http.StatusOK)
		})
	}
}

func TestTCRA35_The256TokenCapCountsLocalTokensRefusesRemoteAdds(t *testing.T) {
	s := newTokenStack(t)
	var local []string
	for i := 0; len(s.tokens(t)) < spec.MaxRemoteTokens; i++ {
		n := fmt.Sprintf("l%03d", i)
		if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: n}); err != nil {
			t.Fatal(err)
		}
		local = append(local, n)
	}
	sum := s.storeSum(t)

	// Step 1.
	r := s.add(t, s.admin, "over", "")
	m := wantTokenProblem(t, r, http.StatusConflict, problem.TokenLimitReached)
	if m["maxTokens"] != float64(spec.MaxRemoteTokens) {
		t.Errorf("maxTokens %v, want %d", m["maxTokens"], spec.MaxRemoteTokens)
	}
	noSecretIn(t, "the refusal", r.body)
	if s.storeSum(t) != sum {
		t.Error("the refused add changed tokens.json")
	}

	// Step 2.
	if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: "local"}); err != nil {
		t.Fatalf("local create past the remote cap: %v", err)
	}
	if n := len(s.tokens(t)); n != spec.MaxRemoteTokens+1 {
		t.Errorf("%d tokens, want %d", n, spec.MaxRemoteTokens+1)
	}

	// Step 3.
	wantTokenProblem(t, s.add(t, s.admin, "over2", ""), http.StatusConflict, problem.TokenLimitReached)

	// The refusals were not counted: 10 removes still succeed.
	for _, n := range local[:spec.TokenMutationsPerMinute] {
		s.remove(t, s.admin, n).want(t, http.StatusOK)
	}
}

func TestTCRA36_AuditFactsForTokenAddAndRemoveCarryMetadataAnd(t *testing.T) {
	s := newTokenStack(t)

	// Steps 1 and 2.
	ci := s.added(t, "ci", `{"scopes":["a:read","b:write"],"expires":"1h"}`)
	ne := s.added(t, "ne", "")
	hashes := s.storeHashes(t)

	// Step 3.
	s.remove(t, s.admin, "ci").want(t, http.StatusOK)

	// Step 4.
	addID := spec.TokensAdd.Op().ID
	wantFacts(t, s.completedWith(t, addID, "ci").Facts, map[string]string{"tokenId": ci.TokenID,
		"tokenName": "ci", "scopes": "a:read,b:write", "expires": s.t0.Add(time.Hour).Format(time.RFC3339)})
	wantFacts(t, s.completedWith(t, addID, "ne").Facts, map[string]string{"tokenId": ne.TokenID,
		"tokenName": "ne", "expires": "never"})
	wantFacts(t, s.completedWith(t, spec.TokensRemove.Op().ID, "ci").Facts, map[string]string{
		"tokenId": ci.TokenID, "tokenName": "ci", "revokedSelf": "false"})

	allowed := []string{"name", "scopes", "expires"}
	for _, r := range s.auditOf(t, addID) {
		if r.Outcome != audit.OutcomeRequested {
			continue
		}
		params := decodeJSON[map[string]any](t, r.Params)
		for k := range params {
			if !slices.Contains(allowed, k) {
				t.Errorf("requested params hold %q: %s", k, r.Params)
			}
		}
		if params["name"] == "ci" && len(params) != 3 {
			t.Errorf("ci's requested params %s, want name, scopes and expires", r.Params)
		}
	}

	var raw []byte
	ents, err := os.ReadDir(s.auditDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(s.auditDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
	}
	if !strings.Contains(string(raw), ci.TokenID) {
		t.Fatal("the audit file does not name ci's tokenId: wrong file?")
	}
	for what, secret := range map[string]string{"ci's secret": ci.Token, "ne's secret": ne.Token,
		"ci's hash": hashes["ci"], "ne's hash": hashes["ne"]} {
		if secret == "" {
			t.Fatalf("%s is empty", what)
		}
		if n := strings.Count(string(raw), secret); n != 0 {
			t.Errorf("the audit file holds %s %d times", what, n)
		}
		if _, b64, ok := strings.Cut(strings.TrimPrefix(secret, "nxs_"), "_"); ok &&
			strings.Contains(string(raw), b64) {
			t.Errorf("the audit file holds the secret part of %s", what)
		}
	}
}

func TestTCRAE8_ConcurrentRemoteAddsSerializeThroughTokenGuard(t *testing.T) {
	s := newTokenStack(t)
	before := len(s.tokens(t))

	statuses := make(chan int, 20)
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				fmt.Sprintf("%s/v1/providers/nexusd/tokens/c%02d/add", s.url, i), nil)
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Authorization", "Bearer "+s.admin)
			resp, err := s.http.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				errs <- err
				return
			}
			statuses <- resp.StatusCode
		})
	}
	wg.Wait()
	close(statuses)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	count := map[int]int{}
	for st := range statuses {
		count[st]++
	}
	if count[http.StatusOK] != 10 || count[http.StatusTooManyRequests] != 10 {
		t.Errorf("statuses %v, want 10 x 200 and 10 x 429", count)
	}
	if n := len(s.tokens(t)); n != before+10 {
		t.Errorf("%d tokens, want %d", n, before+10)
	}
	s.storeHashes(t) // tokens.json is valid JSON
}

func TestTCRAE9_AnAdminMayMintNexusdAdminAndAnAddWithNoExpiry(t *testing.T) {
	s := newTokenStack(t)

	// Step 1.
	r := s.add(t, s.admin, "rot", `{"scopes":["nexusd:admin","nexusd:read"]}`).want(t, http.StatusOK)
	m := decodeJSON[map[string]any](t, r.body)
	if e, ok := m["expires"]; !ok || e != nil {
		t.Errorf("expires = %v (present %v), want null", e, ok)
	}
	rot := decodeJSON[spec.TokenAddResult](t, r.body)
	if rec := s.tokenNamed(t, "rot"); rec == nil || rec.Expires != nil {
		t.Errorf("record %+v, want one without expires", rec)
	}
	b, err := os.ReadFile(filepath.Join(s.stateDir, auth.TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, rec := range f.Tokens {
		if _, has := rec["expires"]; rec["name"] == "rot" && has {
			t.Errorf("rot's record has expires: %v", rec)
		}
	}

	// Step 2.
	s.add(t, rot.Token, "rot2", "").want(t, http.StatusOK)
}

func TestTCRAERR12_TokenAddAndRemoveMapStoreErrorsToProblemsWithout(t *testing.T) {
	s := newTokenStack(t)
	a := s.added(t, "a", "")
	sum := s.storeSum(t)
	var hashes []string
	for _, h := range s.storeHashes(t) {
		hashes = append(hashes, h)
	}
	check := func(what string, r response) {
		t.Helper()
		noSecretIn(t, what, r.body, append([]string{a.Token}, hashes...)...)
		if s.storeSum(t) != sum {
			t.Errorf("%s changed tokens.json", what)
		}
	}

	// Step 1.
	r := s.add(t, s.admin, "a", "")
	m := wantTokenProblem(t, r, http.StatusConflict, problem.Conflict)
	// The detail names the existing token by its ID, never by the supplied
	// name (SEC-1: a name can be a near-variant of a secret).
	if d, _ := m["detail"].(string); !strings.Contains(d, s.idOf(t, "a")) || strings.Contains(d, `"a"`) {
		t.Errorf("conflict detail %q does not name a's ID, or echoes the name", d)
	}
	check("the duplicate add", r)

	// Step 2.
	r = s.add(t, s.admin, "b", `{"expires":"0s"}`)
	m = wantTokenProblem(t, r, http.StatusBadRequest, problem.ValidationFailed)
	_, perr := auth.ParseLifetime("0s")
	if perr == nil {
		t.Fatal("ParseLifetime accepts 0s")
	}
	errsJSON, err := json.Marshal(m["errors"])
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSON[[]problem.FieldError](t, errsJSON)
	want := []problem.FieldError{{Path: "/expires", Message: perr.Error()}}
	if !slices.Equal(got, want) {
		t.Errorf("errors %+v, want %+v", got, want)
	}
	check("the bad expiry", r)

	// Step 3.
	r = s.remove(t, s.admin, "nope")
	m = wantTokenProblem(t, r, http.StatusNotFound, problem.NotFound)
	if d, _ := m["detail"].(string); strings.Contains(d, "nope") {
		t.Errorf("not-found detail %q echoes the reference", d)
	}
	check("the unknown remove", r)
}
