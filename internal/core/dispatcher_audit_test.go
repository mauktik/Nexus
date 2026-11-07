package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

type auditIn struct {
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
}

var auditedRestart = def.Define[auditIn, auditIn](def.Op{
	ID: "audits.things.restart", Provider: "audits", Kind: def.KindResourceAction, ResourceType: "things",
	Action: "restart", Summary: "s", Description: "Mutating operation for the dispatcher audit tests.",
	Scope: def.Scope{Name: "audits:write"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	Sensitive: []string{"token"},
	CLI:       def.CLIBinding{Noun: "thing", Verb: "restart", Positional: "name"},
	Refine: func(s *jsonschema.Schema) {
		s.Properties["name"].Pattern = `^[a-z0-9-]+$`
		s.Properties["token"].Pattern = `^[a-z]+$`
	},
})

type auditsProvider struct{ state apitypes.ProviderState }

func (a auditsProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "audits", ReadScope: "audits:read", Ops: []def.Op{auditedRestart.Op()}}
}
func (auditsProvider) Configure(func(any) error) error { return nil }
func (a auditsProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: a.state, Reason: "test"}
}
func (auditsProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.InProcess(auditedRestart, provider.InProcessSpec[auditIn, auditIn]{
		Run: func(_ context.Context, in auditIn, _ *provider.Run) (auditIn, error) { return in, nil },
	})}
}

func auditFixture(t *testing.T, state apitypes.ProviderState) (*Dispatcher, *audit.Log, *atomic64) {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	reg, err := New([]provider.Provider{auditsProvider{state: state}}, Options{
		Logger: log, Audit: lg, Providers: map[string]ProviderConfig{"audits": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handled := &atomic64{}
	reg.entries = append(reg.entries, Entry{Provider: def.SystemProvider, Binding: provider.Sync(systemspec.OperationsCancel,
		func(_ context.Context, in systemspec.OperationInput) (apitypes.Operation, error) {
			handled.n++
			return apitypes.Operation{ID: in.ID}, nil
		})})
	load := func(context.Context, string) (TargetRecord, error) {
		return TargetRecord{Provider: "audits", StartScope: "audits:write"}, nil
	}
	return NewDispatcher(reg, DispatcherOptions{Logger: log, LoadTarget: load}), lg, handled
}

type atomic64 struct{ n int64 }

func records(t *testing.T, lg *audit.Log) []systemspec.AuditRecord {
	t.Helper()
	page, err := lg.Query(audit.Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(page.Items) // Oldest first
	return page.Items
}

// Every refusal of an authenticated mutating request after the scope check
// is audited refused with its problem slug; a validation refusal holds the
// failing paths and no values. Reads are not audited.
func TestDispatcherAuditsRefusals(t *testing.T) {
	d, lg, _ := auditFixture(t, apitypes.ProviderEnabled)
	who := auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "ops", Scopes: []string{"audits:write"},
		Source: netip.MustParseAddr("192.0.2.7")}
	invoke := func(input string) error {
		_, err := d.Invoke(context.Background(), InvokeRequest{Caller: who, Surface: SurfaceREST,
			Op: auditedRestart.Op(), Input: json.RawMessage(input)})
		return err
	}
	const secret = "SENTINELVALUE"
	problemOf(t, invoke(`{"name":"web-1","token":"`+secret+`","extra":"`+secret+`"}`), http.StatusBadRequest)
	problemOf(t, invoke(`{"name":"web-1"}`), http.StatusInternalServerError) // Long-running stub until 3.2

	recs := records(t, lg)
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want 2", recs)
	}
	v := recs[0]
	if v.Outcome != audit.OutcomeRefused || v.Problem != string(problem.ValidationFailed) || v.Target != "web-1" ||
		!slices.Equal(v.InvalidFields, []string{"/extra", "/token"}) || len(v.Params) != 0 ||
		v.TokenName != "ops" || v.TokenID != "a1b2c3d4e5f6" || v.Source != "192.0.2.7" || v.Surface != "rest" ||
		v.Provider != "audits" || v.Action != "audits.things.restart" {
		t.Fatalf("validation refusal = %+v", v)
	}
	if recs[1].Outcome != audit.OutcomeRefused || recs[1].Problem != string(problem.InternalError) {
		t.Fatalf("stub refusal = %+v", recs[1])
	}
	page, _ := lg.Query(audit.Query{})
	for _, r := range page.Items {
		if b, _ := json.Marshal(r); strings.Contains(string(b), secret) {
			t.Fatalf("an unvalidated value reached the audit log: %s", b)
		}
	}

	// A provider that is not enabled: capability-unavailable, refused.
	d2, lg2, _ := auditFixture(t, apitypes.ProviderUnavailable)
	_, err := d2.Invoke(context.Background(), InvokeRequest{Caller: who, Op: auditedRestart.Op(),
		Input: json.RawMessage(`{"name":"bad name"}`)})
	problemOf(t, err, http.StatusServiceUnavailable)
	if recs := records(t, lg2); len(recs) != 1 || recs[0].Problem != string(problem.CapabilityUnavailable) || recs[0].Target != "" {
		t.Fatalf("capability refusal = %+v (a target that fails its schema is not recorded)", recs)
	}
}

// A sync mutating op writes requested, with redacted parameters, before
// its handler runs; when the write fails the call is refused with
// audit-unavailable and the handler never runs.
func TestRequestedIsWrittenBeforeTheHandlerAndFailsClosed(t *testing.T) {
	d, lg, handled := auditFixture(t, apitypes.ProviderEnabled)
	who := auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "ops", Scopes: []string{"audits:write"}}
	cancel := systemspec.OperationsCancel.Op()
	if err := call(d, cancel, recID, "audits:write"); err != nil {
		t.Fatal(err)
	}
	recs := records(t, lg)
	if len(recs) != 1 || recs[0].Outcome != audit.OutcomeRequested || recs[0].OperationID != recID ||
		recs[0].Action != cancel.ID || string(recs[0].Params) != `{"id":"`+recID+`"}` || handled.n != 1 {
		t.Fatalf("records = %+v, handled %d", recs, handled.n)
	}

	_, err := d.Invoke(context.Background(), InvokeRequest{Caller: who, Op: cancel,
		Input: json.RawMessage(`{"id":"` + recID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	p := problemOf(t, call(d, cancel, recID, "audits:write"), http.StatusServiceUnavailable)
	if p.Slug() != problem.AuditUnavailable || handled.n != 2 {
		t.Fatalf("after an audit failure: %v, handled %d; want audit-unavailable and no handler call", p, handled.n)
	}
}
