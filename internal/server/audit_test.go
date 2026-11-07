package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/systemspec"
)

var (
	ledgerGet = def.Define[widgetIn, gadgetOut](def.Op{
		ID: "ledgers.entries.get", Provider: "ledgers", Kind: def.KindGet, ResourceType: "entries",
		Summary: "s", Description: "Read operation for the audit tests.",
		Scope: def.Scope{Name: "ledgers:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "entry", Verb: "get", Positional: "name"},
	})
	ledgerRestart = def.Define[widgetIn, gadgetOut](def.Op{
		ID: "ledgers.entries.restart", Provider: "ledgers", Kind: def.KindResourceAction, ResourceType: "entries",
		Action: "restart", Summary: "s", Description: "Mutating operation for the audit tests.",
		Scope: def.Scope{Name: "ledgers:write"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "entry", Verb: "restart", Positional: "name"},
	})
)

type ledgers struct{}

func (ledgers) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "ledgers", ReadScope: "ledgers:read",
		Ops: []def.Op{ledgerGet.Op(), ledgerRestart.Op()}}
}

func (ledgers) Configure(func(any) error) error { return nil }

func (ledgers) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (ledgers) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(ledgerGet, func(_ context.Context, in widgetIn) (gadgetOut, error) {
			return gadgetOut(in), nil
		}),
		provider.InProcess(ledgerRestart, provider.InProcessSpec[widgetIn, gadgetOut]{
			Run: func(_ context.Context, in widgetIn, _ *provider.Run) (gadgetOut, error) { return gadgetOut(in), nil },
		}),
	}
}

// auditedServer is the real handler chain with an audit log shared by the
// authenticator and the Dispatcher, as the daemon wires them.
func auditedServer(t *testing.T, tokens string, clk *clock) (http.Handler, *audit.Log, string) {
	t.Helper()
	dir := t.TempDir()
	lg, err := audit.Open(audit.Options{Dir: dir, Host: "testhost", Now: clk.Now, Logger: discardLog,
		AggregateWindow: auth.FailureWindow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lg.Close(); err != nil {
			t.Error(err)
		}
	})
	reg, err := core.New([]provider.Provider{ledgers{}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: discardLog, Audit: lg,
		Providers: map[string]core.ProviderConfig{"ledgers": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.New(tokens, auth.Options{Now: clk.Now, Logger: discardLog, Audit: lg})
	srv, err := server.New(reg, server.Options{Auth: authn, Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), lg, dir
}

func serveMethod(h http.Handler, method, peer, path, authz string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = peer
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func allRecords(t *testing.T, lg *audit.Log) []systemspec.AuditRecord {
	t.Helper()
	page, err := lg.Query(audit.Query{Limit: systemspec.MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.NextCursor != "" {
		t.Fatalf("more than %d records", systemspec.MaxLimit)
	}
	return page.Items
}

// TC-34: denials are audited and reads are not.
func TestDenialsAreAuditedAndReadsAreNot(t *testing.T) {
	tokens := t.TempDir()
	reader := mint(t, tokens, "reader", "ledgers:read")
	invalid := "nxs_000000000000_" + strings.Repeat("Q", 43)
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	h, lg, dir := auditedServer(t, tokens, clk)
	const mutate = "/v1/providers/ledgers/entries/e1/restart"

	cases := []struct {
		peer, authz string
		status      int
	}{
		{"192.0.2.31:4000", "", http.StatusUnauthorized},
		{"192.0.2.32:4000", "Bearer " + invalid, http.StatusUnauthorized},
		{"[2001:db8::33]:4000", "Bearer " + reader, http.StatusForbidden},
	}
	for _, c := range cases {
		if w := serveMethod(h, http.MethodPost, c.peer, mutate, c.authz); w.Code != c.status {
			t.Fatalf("POST from %s: %d %s, want %d", c.peer, w.Code, w.Body, c.status)
		}
	}
	for i := range 5 {
		if w := serveMethod(h, http.MethodGet, "192.0.2.40:4000", "/v1/providers/ledgers/entries/e1", "Bearer "+reader); w.Code != http.StatusOK {
			t.Fatalf("read %d: %d %s", i, w.Code, w.Body)
		}
	}

	recs := allRecords(t, lg)
	if len(recs) != 3 {
		t.Fatalf("%d records, want exactly 3 denials: %+v", len(recs), recs)
	}
	// Newest first.
	forbidden, invalidRec, none := recs[0], recs[1], recs[2]
	for _, r := range []systemspec.AuditRecord{none, invalidRec} {
		if r.Outcome != audit.OutcomeDenied || r.Problem != "unauthorized" || r.TokenID != "" || r.TokenName != "" ||
			r.Surface != "rest" || r.Host != "testhost" || r.ID == "" {
			t.Fatalf("authentication denial = %+v", r)
		}
	}
	if none.Source != "192.0.2.31" || invalidRec.Source != "192.0.2.32" {
		t.Fatalf("sources = %q, %q", none.Source, invalidRec.Source)
	}
	parts := strings.SplitN(reader, "_", 3) // nxs, ID, secret; the secret may hold '_'
	readerID := parts[1]
	if forbidden.Outcome != audit.OutcomeDenied || forbidden.Problem != "forbidden" || forbidden.Source != "2001:db8::33" ||
		forbidden.TokenName != "reader" || forbidden.TokenID != readerID ||
		forbidden.Provider != "ledgers" || forbidden.Action != "ledgers.entries.restart" || forbidden.Target != "e1" {
		t.Fatalf("authorization denial = %+v", forbidden)
	}

	raw, err := os.ReadFile(filepath.Join(dir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{reader, parts[2], invalid, strings.Repeat("Q", 43)} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("token material %q is in the audit log", secret)
		}
	}
}

// TC-36: rate-limited denials collapse into aggregate records.
func TestRateLimitedDenialsAggregate(t *testing.T) {
	tokens := t.TempDir()
	bad := "Bearer nxs_000000000000_" + strings.Repeat("A", 43)
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	h, lg, _ := auditedServer(t, tokens, clk)

	flood := func(peer string, n int) {
		t.Helper()
		limited := 0
		for range n {
			if w := serveMethod(h, http.MethodGet, peer, "/v1/capabilities", bad); w.Code == http.StatusTooManyRequests {
				limited++
			}
			clk.t = clk.t.Add(10 * time.Millisecond)
		}
		if limited != n-auth.DefaultFailureLimitPerMinute {
			t.Fatalf("%d of %d attempts rate limited", limited, n)
		}
	}
	const attempts = 1000
	flood("192.0.2.50:4000", attempts)
	clk.t = clk.t.Add(auth.FailureWindow)
	recs := allRecords(t, lg)

	individual, sum, aggregates := 0, 0, 0
	for _, r := range recs {
		if r.Source != "192.0.2.50" || r.Outcome != audit.OutcomeDenied {
			t.Fatalf("unexpected record %+v", r)
		}
		switch r.Problem {
		case "unauthorized":
			if r.Count != 0 {
				t.Fatalf("individual denial with a count: %+v", r)
			}
			individual++
		case "rate-limited":
			aggregates++
			sum += r.Count
		default:
			t.Fatalf("unexpected record %+v", r)
		}
	}
	if individual != auth.DefaultFailureLimitPerMinute || sum != attempts-individual {
		t.Fatalf("%d individual denials and aggregate counts summing to %d; want %d and %d",
			individual, sum, auth.DefaultFailureLimitPerMinute, attempts-auth.DefaultFailureLimitPerMinute)
	}
	if aggregates != 1 {
		t.Fatalf("%d aggregate records for one window, want 1", aggregates)
	}

	// Five times the attempts from an IPv6 /64 cost the same number of
	// records: the count, not the record count, grows.
	flood("[2001:db8:0:7::1]:4000", 5*attempts)
	clk.t = clk.t.Add(auth.FailureWindow)
	var v6 []systemspec.AuditRecord
	for _, r := range allRecords(t, lg) {
		if strings.HasPrefix(r.Source, "2001:db8:0:7:") {
			v6 = append(v6, r)
		}
	}
	if len(v6) != auth.DefaultFailureLimitPerMinute+1 {
		t.Fatalf("%d records for %d attempts, want %d", len(v6), 5*attempts, auth.DefaultFailureLimitPerMinute+1)
	}
	if agg := v6[0]; agg.Problem != "rate-limited" || agg.Source != "2001:db8:0:7::/64" ||
		agg.Count != 5*attempts-auth.DefaultFailureLimitPerMinute {
		t.Fatalf("IPv6 aggregate = %+v", agg)
	}
}
