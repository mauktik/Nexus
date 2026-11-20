// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-39, TC-RA-40
// and TC-RA-41 (WI 2.4): the token ops through the real server and
// Dispatcher with the real nexusd provider, not its catalog stub.

package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	nexusdprov "github.com/mauktik/nexus/internal/providers/nexusd"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// tokenDaemon is the provider.Daemon the real nexusd provider is handed:
// only its state directory matters to the token ops.
type tokenDaemon struct{ stateDir string }

func (d tokenDaemon) ConfigPath() string   { return filepath.Join(d.stateDir, "nexusd.yaml") }
func (d tokenDaemon) StateDir() string     { return d.stateDir }
func (d tokenDaemon) StartedAt() time.Time { return time.Time{} }
func (d tokenDaemon) CheckConfig() provider.ConfigCheck {
	return provider.ConfigCheck{}
}
func (d tokenDaemon) CheckConfigData([]byte) provider.ConfigCheck {
	return provider.ConfigCheck{}
}
func (d tokenDaemon) Reload(context.Context) (provider.ReloadResult, error) {
	return provider.ReloadResult{}, nil
}

// tokenStack is the conformance server over the real nexusd provider, with
// its real clock, sharing one state directory with the authenticator.
type tokenStack struct {
	*stack
	stateDir string
	admin    string // nexusd:admin only
	reader   string // nexusd:read only
}

const (
	tokenAdminName  = "ra-admin"
	tokenReaderName = "ra-reader"
)

func newTokenStack(t *testing.T) *tokenStack {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: "testhost", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: filepath.Join(stateDir, "ops"), Host: "testhost",
		Logger: log, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	p := nexusdprov.New(nexusdprov.Options{Host: "testhost"})
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: "testhost", Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{nexusd.Name: {Enabled: true}}})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	p.UseDaemon(tokenDaemon{stateDir: stateDir})

	mint := func(name string, scopes ...string) string {
		tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
		if err != nil {
			t.Fatalf("token %s: %v", name, err)
		}
		return tok
	}
	s := &tokenStack{stack: &stack{reg: reg, audit: alog, exact: map[string]string{}, exactName: map[string]string{}},
		stateDir: stateDir}
	s.admin = mint(tokenAdminName, nexusd.AdminScope)
	s.reader = mint(tokenReaderName, nexusd.ReadScope)
	s.full, s.none = s.admin, mint(noneToken)
	s.exact[nexusd.AdminScope], s.exactName[nexusd.AdminScope] = s.admin, tokenAdminName
	s.exact[nexusd.ReadScope], s.exactName[nexusd.ReadScope] = s.reader, tokenReaderName

	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(stateDir, auth.Options{Logger: log}),
		NoteLimitPerMinute: 10000})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	s.base = "http://" + ln.Addr().String()
	return s
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

// unauthenticated sends op's sample input with no Authorization header.
func (s *tokenStack) unauthenticated(t *testing.T, op def.Op, in map[string]any) response {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := in["name"].(string); ok {
		path = strings.Replace(path, "{name}", name, 1)
	}
	req, err := http.NewRequest(method, s.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, body: b}
}

func tokenOps() []def.Op {
	return []def.Op{nexusd.TokensList.Op(), nexusd.TokensAdd.Op(), nexusd.TokensRemove.Op()}
}

// TC-RA-39: Every token op refuses an unauthenticated call and a nexusd:read-only call.
func TestTCRA39_EveryTokenOpRefusesAnUnauthenticatedCallAndA(t *testing.T) {
	s := newTokenStack(t)
	// A token for remove to find, so a call that reached the handler would
	// change the store.
	if _, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: "victim"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range tokenOps() {
		in := map[string]any{}
		switch op.ID {
		case nexusd.TokensAdd.Op().ID:
			in["name"] = "intruder"
		case nexusd.TokensRemove.Op().ID:
			in["name"] = "victim"
		}

		// Step 1: no token.
		sum := s.storeSum(t)
		r := s.unauthenticated(t, op, in)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s with no token: %d %.200s, want 401", op.ID, r.status, r.body)
		} else if p := r.problem(t); p.Slug() != problem.Unauthorized {
			t.Errorf("%s with no token: problem %s", op.ID, p.Slug())
		}
		if got := s.storeSum(t); got != sum {
			t.Errorf("%s with no token changed tokens.json", op.ID)
		}
		if recs := s.records(t, audit.Query{Action: op.ID}); len(recs) != 0 {
			t.Errorf("%s with no token reached the Dispatcher: audit records %q", op.ID, recs)
		}

		// Step 2: a nexusd:read token.
		r = s.call(t, op, s.reader, in)
		if r.status != http.StatusForbidden {
			t.Errorf("%s with %s: %d %.200s, want 403", op.ID, nexusd.ReadScope, r.status, r.body)
		} else if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != nexusd.AdminScope {
			t.Errorf("%s: problem %s, requiredScope %v, want forbidden naming %s", op.ID, p.Slug(),
				p.Extensions["requiredScope"], nexusd.AdminScope)
		}
		if got := s.storeSum(t); got != sum {
			t.Errorf("%s with %s changed tokens.json", op.ID, nexusd.ReadScope)
		}
		denied := s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied, TokenName: tokenReaderName})
		requested := s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeRequested})
		if len(denied) != 1 || len(requested) != 0 {
			t.Errorf("%s with %s: denied records %q and requested records %q, want 1 and 0", op.ID,
				nexusd.ReadScope, denied, requested)
		}
	}
	if l, err := auth.List(s.stateDir); err != nil || !slices.ContainsFunc(l, func(tk auth.Token) bool { return tk.Name == "victim" }) {
		t.Errorf("victim token gone: %v %v", l, err)
	}
}

// TC-RA-40: The FR-25 negative-input suite walks the token string fields.
func TestTCRA40_TheFR25NegativeInputSuiteWalksTheTokenString(t *testing.T) {
	// Step 1: the (op, field) pairs TestAttackStringsAgainstEveryStringParameter
	// attacks: every string parameter of every op its stack serves.
	walked := map[string]bool{}
	for _, op := range newStack(t).ops() {
		for _, path := range stringParams(t, op) {
			walked[op.ID+" "+path] = true
		}
	}
	add, remove := nexusd.TokensAdd.Op(), nexusd.TokensRemove.Op()
	want := []struct {
		op   def.Op
		path string
	}{{add, "/name"}, {add, "/scopes/*"}, {add, "/expires"}, {remove, "/name"}}
	for _, w := range want {
		if !walked[w.op.ID+" "+w.path] {
			t.Errorf("the walker does not attack %s %s", w.op.ID, w.path)
		}
		if inertAllowed[w.op.ID+" "+w.path] {
			t.Errorf("%s %s is inert-allowed: its attacks are not refused", w.op.ID, w.path)
		}
	}

	// Every attack on those fields, through the real provider, is refused
	// with 400 validation-failed and changes nothing.
	s := newTokenStack(t)
	sm := sampler{}
	sum := s.storeSum(t)
	for _, w := range want {
		base, _ := sm.value(t, w.op, "", inputSchema(t, w.op)).(map[string]any)
		for _, a := range attacksFor(w.op) {
			t.Run(fmt.Sprintf("%s%s/%s", w.op.ID, w.path, a.name), func(t *testing.T) {
				r := s.call(t, w.op, s.admin, sm.withValue(t, w.op, base, w.path, a.value))
				if r.status != http.StatusBadRequest {
					t.Fatalf("status %d %.300s, want 400", r.status, r.body)
				}
				if p := r.problem(t); p.Slug() != problem.ValidationFailed {
					t.Errorf("problem %s, want %s", p.Slug(), problem.ValidationFailed)
				}
				if got := s.storeSum(t); got != sum {
					t.Errorf("tokens.json changed")
				}
			})
		}
	}
}

// TC-RA-41: The 429 path through the real provider sets Retry-After, writes a refused record and changes nothing.
func TestTCRA41_The429PathThroughTheRealProviderSetsRetryAfter(t *testing.T) {
	s := newTokenStack(t)
	add := nexusd.TokensAdd.Op()
	start := time.Now()

	// Step 1: ten successful adds.
	for i := range nexusd.TokenMutationsPerMinute {
		r := s.call(t, add, s.admin, map[string]any{"name": fmt.Sprintf("rl-%d", i)})
		if r.status != http.StatusOK {
			t.Fatalf("add %d: %d %.300s", i, r.status, r.body)
		}
	}
	before, err := auth.List(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sum := s.storeSum(t)

	// Step 2: the eleventh.
	method, path, err := openapi.Route(add)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, s.base+strings.Replace(path, "{name}", "rl-over", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.admin)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= time.Minute {
		t.Fatalf("the adds took %s, longer than the window", time.Since(start))
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("eleventh add: %d %.300s, want 429", resp.StatusCode, body)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 60 {
		t.Errorf("Retry-After %q, want an integer from 1 to 60", resp.Header.Get("Retry-After"))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type %q, want application/problem+json", ct)
	}
	if p := (response{status: resp.StatusCode, body: body}).problem(t); p.Slug() != problem.RateLimited {
		t.Errorf("problem %s, want %s", p.Slug(), problem.RateLimited)
	}
	refused := s.records(t, audit.Query{Action: add.ID, Outcome: audit.OutcomeRefused})
	if len(refused) != 1 || !strings.HasSuffix(refused[0], " "+string(problem.RateLimited)) {
		t.Errorf("refused records %q, want one with problem %s", refused, problem.RateLimited)
	}
	after, err := auth.List(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || s.storeSum(t) != sum {
		t.Errorf("auth.List length %d -> %d, or tokens.json changed", len(before), len(after))
	}
}
