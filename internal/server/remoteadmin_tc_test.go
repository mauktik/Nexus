// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-13, TC-RA-14
// and TC-RA-112 (WI 1.4): what the wire carries for a SecretOutput op, a
// 429 from a sync handler, and a Done answer at admission.

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// raSecret is the credential the SecretOutput op returns once.
const raSecret = "RA-SECRET-1"

type raNameIn struct {
	Name string `json:"name"`
}

type raIssueOut struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type raEmpty struct{}

type raSetIn struct {
	X string `json:"x,omitempty"`
}

type raSetOut struct {
	Changed bool   `json:"changed"`
	Value   string `json:"value"`
}

func raOp(id string, kind def.Kind, rt, action string, risk def.Risk, mode def.Mode, noun, verb string) def.Op {
	return def.Op{
		ID: id, Provider: "ra", Kind: kind, ResourceType: rt, Action: action,
		Summary: "s", Description: "Operation for the remote-admin server tests.",
		Scope: def.Scope{Name: "ra:admin"}, Risk: risk, Mode: mode,
		CLI: def.CLIBinding{Noun: noun, Verb: verb},
	}
}

var (
	// raIssue is the planted SyncMutation+SecretOutput op (TC-RA-2).
	raIssue = def.Define[raNameIn, raIssueOut](func() def.Op {
		op := raOp("ra.tokens.issue", def.KindResourceAction, "tokens", "issue", def.RiskMutating, def.ModeSync, "ratoken", "add")
		op.SyncMutation, op.SecretOutput, op.CLI.Positional = true, "token", "name"
		return op
	}())
	// raThrottled is a SyncMutation whose handler answers 429.
	raThrottled = def.Define[raEmpty, raEmpty](func() def.Op {
		op := raOp("ra.actions.throttled", def.KindProviderAction, "", "throttled", def.RiskMutating, def.ModeSync, "rathrottle", "invoke")
		op.SyncMutation = true
		return op
	}())
	// raRead is a plain sync read.
	raRead = def.Define[raNameIn, raIssueOut](func() def.Op {
		op := raOp("ra.tokens.get", def.KindGet, "tokens", "", def.RiskReadOnly, def.ModeSync, "ratoken", "get")
		op.Scope, op.CLI.Positional = def.Scope{Name: "ra:read"}, "name"
		return op
	}())
	// raSet is TC-RA-111's op D: detached, CompletesAtAdmission.
	raSet = def.Define[raSetIn, raSetOut](func() def.Op {
		op := raOp("ra.actions.set", def.KindProviderAction, "", "set", def.RiskMutating, def.ModeLongRunning, "raset", "update")
		op.CompletesAtAdmission = true
		return op
	}())
)

type raProvider struct{}

func (raProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "ra", ReadScope: "ra:read",
		Ops: []def.Op{raIssue.Op(), raThrottled.Op(), raRead.Op(), raSet.Op()}}
}

func (raProvider) Configure(func(any) error) error { return nil }

func (raProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (raProvider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(raIssue, func(_ context.Context, in raNameIn) (raIssueOut, error) {
			return raIssueOut{Name: in.Name, Token: raSecret}, nil
		}),
		provider.Sync(raThrottled, func(context.Context, raEmpty) (raEmpty, error) {
			p := problem.New(problem.RateLimited, "x")
			p.RetryAfter = 7300 * time.Millisecond
			return raEmpty{}, p
		}),
		provider.Sync(raRead, func(_ context.Context, in raNameIn) (raIssueOut, error) {
			return raIssueOut{Name: in.Name}, nil
		}),
		provider.Detached(raSet, provider.DetachedSpec[raSetIn, raSetOut]{
			// Done when nothing would change; anything else needs an operation.
			Admission: func(_ context.Context, in raSetIn) (provider.Admitted[raSetIn, raSetOut], error) {
				if in.X == "same" {
					return provider.Admitted[raSetIn, raSetOut]{Done: true, Out: raSetOut{Value: in.X}}, nil
				}
				return provider.Admitted[raSetIn, raSetOut]{In: in}, nil
			},
			Prepare: func(context.Context, raSetIn, *provider.Run) (detach.Spec, error) {
				return detach.Spec{Argv: []string{"/bin/true"}, KillMode: detach.KillProcess, MaxRuntime: time.Hour}, nil
			},
			Finalize: func(_ context.Context, _ raSetIn, x detach.Exit, _ *provider.Run) (raSetOut, error) {
				return raSetOut{Changed: x.ExitCode == 0}, nil
			},
		}),
	}
}

// startRA runs the real server over raProvider with an audit log and an
// operation manager whose launcher starts nothing. It returns the server
// and a token holding ra:admin and ra:read.
func startRA(t *testing.T) (*testServer, string) {
	t.Helper()
	tokens := t.TempDir()
	tok := mint(t, tokens, "ra", "ra:admin", "ra:read")
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "testhost", Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lg.Close(); err != nil {
			t.Error(err)
		}
	})
	state := t.TempDir()
	st, err := store.Open(state, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	opsDir := filepath.Join(state, "ops")
	launcher, err := detach.New(detach.Config{OpsDir: opsDir, Executable: "/bin/true", Logger: discardLog}, detach.Options{
		Launch: func(context.Context, detach.Invocation) error { return nil },
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal:       func(context.Context, string, syscall.Signal) error { return nil },
		ListUnits:    func(context.Context) ([]string, error) { return nil, nil },
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: lg, Dir: opsDir, Host: "testhost", Logger: discardLog, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
	})
	ts := startServerWith(t, server.Options{Auth: auth.New(tokens, auth.Options{Logger: discardLog})}, core.Options{
		Audit: lg, Ops: mgr, Providers: map[string]core.ProviderConfig{"ra": {Enabled: true}},
	}, raProvider{})
	return ts, tok
}

func raPost(t *testing.T, ts *testServer, tok, path, body string) (*http.Response, []byte) {
	t.Helper()
	return request(t, http.MethodPost, ts.base+path, tok, strings.NewReader(body))
}

func noStore(resp *http.Response) bool {
	for _, v := range resp.Header.Values("Cache-Control") {
		if strings.Contains(v, "no-store") {
			return true
		}
	}
	return false
}

// TC-RA-13: A SecretOutput route answers with Cache-Control no-store, and other routes do not. Owner WI 1.4. Requirements: NFR-1, CON-3.
func TestTCRA13_ASecretOutputRouteAnswersWithCacheControlNoStore(t *testing.T) {
	ts, tok := startRA(t)
	const issue = "/v1/providers/ra/tokens/t1/issue"

	resp, body := raPost(t, ts, tok, issue, `{}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), raSecret) {
		t.Fatalf("step 1: %d %q, want 200 with the secret", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("step 1: Cache-Control = %q, want no-store", got)
	}

	resp, body = raPost(t, ts, tok, issue, `[1]`)
	if p := decodeProblem(t, resp, body); resp.StatusCode != http.StatusBadRequest || p.Slug() != problem.ValidationFailed {
		t.Fatalf("step 2: %d %+v, want 400 validation-failed", resp.StatusCode, p)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("step 2: Cache-Control = %q, want no-store", got)
	}

	resp, body = request(t, http.MethodGet, ts.base+"/v1/providers/ra/tokens/t1", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3: %d %q, want 200", resp.StatusCode, body)
	}
	if noStore(resp) {
		t.Errorf("step 3: a read that carries no secret has Cache-Control %v", resp.Header.Values("Cache-Control"))
	}
}

// TC-RA-14: A 429 returned by a sync handler reaches the wire with Retry-After and a problem body. Owner WI 1.4. Requirements: FR-5.
func TestTCRA14_A429ReturnedByASyncHandlerReachesTheWireWith(t *testing.T) {
	ts, tok := startRA(t)
	resp, body := raPost(t, ts, tok, "/v1/providers/ra/actions/throttled", `{}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "8" {
		t.Errorf("Retry-After = %q, want 8 (7.3s rounded up)", got)
	}
	if p := decodeProblem(t, resp, body); p.Slug() != problem.RateLimited {
		t.Errorf("slug = %q, want rate-limited", p.Slug())
	}
	var members map[string]any
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatal(err)
	}
	for k := range members {
		if strings.EqualFold(k, "retryAfter") {
			t.Errorf("body has a %s member: %s", k, body)
		}
	}
}

// TC-RA-112: A Done answer reaches the wire as 200 without Location or Retry-After. Owner WI 1.4. Requirements: FR-7, CON-3.
func TestTCRA112_ADoneAnswerReachesTheWireAs200WithoutLocation(t *testing.T) {
	ts, tok := startRA(t)
	const set = "/v1/providers/ra/actions/set"

	resp, body := raPost(t, ts, tok, set, `{"x":"same"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 1: status = %d, want 200; body %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("step 1: Content-Type = %q, want application/json", ct)
	}
	want, err := json.Marshal(raSetOut{Value: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(want) {
		t.Errorf("step 1: body = %s, want %s", body, want)
	}
	for _, h := range []string{"Location", "Retry-After"} {
		if v := resp.Header.Values(h); len(v) > 0 {
			t.Errorf("step 1: %s = %v, want none", h, v)
		}
	}

	resp, body = raPost(t, ts, tok, set, `{"x":"diff"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("step 2: status = %d, want 202; body %q", resp.StatusCode, body)
	}
	var op struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &op); err != nil || op.ID == "" {
		t.Fatalf("step 2: body %q has no operation id: %v", body, err)
	}
	if got, want := resp.Header.Get("Location"), "/v1/operations/"+op.ID; got != want {
		t.Errorf("step 2: Location = %q, want %q", got, want)
	}
}
