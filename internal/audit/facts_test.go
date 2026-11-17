package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

type factsIn struct{}

type factsOut struct {
	Collected bool `json:"collected"`
}

var factsRead = def.Define[factsIn, factsOut](def.Op{
	ID: "fr.things.list", Provider: "fr", Kind: def.KindList, ResourceType: "things",
	Summary: "s", Description: "Read operation for the audit facts tests.",
	Scope: def.Scope{Name: "fr:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
	CLI: def.CLIBinding{Noun: "thing", Verb: "get"},
})

// factsProvider serves factsRead with a handler that adds a fact and
// reports whether the call had a collector.
type factsProvider struct{}

func (factsProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "fr", ReadScope: "fr:read", Ops: []def.Op{factsRead.Op()}}
}
func (factsProvider) Configure(func(any) error) error { return nil }
func (factsProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (factsProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(factsRead, func(ctx context.Context, _ factsIn) (factsOut, error) {
		audit.AddFact(ctx, "read", "fact")
		return factsOut{Collected: audit.Facts(ctx)["read"] == "fact"}, nil
	})}
}

// TC-SM-50 (WI 3.4; FR-16): The facts collector is bounded, is a no-op without a collector, and read ops record no facts
func TestTCSM50_TheFactsCollectorIsBoundedIs(t *testing.T) {
	// Step 1: no collector.
	bg := context.Background()
	audit.AddFact(bg, "a", "b")
	if got := audit.Facts(bg); got != nil {
		t.Fatalf("Facts without a collector = %v, want nil", got)
	}

	// Step 2: oversize key and value are cut to the bounds.
	ctx := audit.WithFacts(bg)
	if got := audit.Facts(ctx); got != nil {
		t.Fatalf("Facts of an empty collector = %v, want nil", got)
	}
	audit.AddFact(ctx, strings.Repeat("k", 100), strings.Repeat("v", 1000))
	got := audit.Facts(ctx)
	if len(got) != 1 {
		t.Fatalf("Facts = %v, want one fact", got)
	}
	for k, v := range got {
		if len(k) != audit.MaxFactKeyBytes || len(v) != audit.MaxFactValueBytes {
			t.Fatalf("stored key %d bytes, value %d bytes; want %d and %d",
				len(k), len(v), audit.MaxFactKeyBytes, audit.MaxFactValueBytes)
		}
	}
	// A cut never splits a rune, and an empty key is ignored.
	audit.AddFact(ctx, strings.Repeat("€", 30), strings.Repeat("€", 100))
	audit.AddFact(ctx, "", "x")
	for k, v := range audit.Facts(ctx) {
		if !utf8.ValidString(k) || !utf8.ValidString(v) || len(k) > audit.MaxFactKeyBytes || len(v) > audit.MaxFactValueBytes || k == "" {
			t.Fatalf("fact %q=%q is not a bounded, valid UTF-8 pair", k, v)
		}
	}
	// The accessor returns a copy.
	audit.Facts(ctx)["injected"] = "x"
	if _, ok := audit.Facts(ctx)["injected"]; ok {
		t.Fatal("Facts returned the collector's own map")
	}

	// Steps 3 and 4: more concurrent adders than MaxFacts, then the
	// accessor; the count is bounded too.
	ctx = audit.WithFacts(bg)
	adds := audit.MaxFacts + 18
	var wg sync.WaitGroup
	for i := range adds {
		wg.Go(func() {
			audit.AddFact(ctx, fmt.Sprintf("key-%03d", i), "value")
			_ = audit.Facts(ctx)
		})
	}
	wg.Wait()
	if n := len(audit.Facts(ctx)); n != audit.MaxFacts {
		t.Fatalf("%d facts after %d distinct adds, want the bound %d", n, adds, audit.MaxFacts)
	}
	// Replacing an existing key is allowed at the bound.
	for k := range audit.Facts(ctx) {
		audit.AddFact(ctx, k, "replaced")
		if audit.Facts(ctx)[k] != "replaced" {
			t.Fatalf("fact %q was not replaced at the bound", k)
		}
		break
	}

	// Step 5: a sync read op through the core writes no audit record, so
	// the fact its handler added appears nowhere.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	reg, err := core.New([]provider.Provider{factsProvider{}}, core.Options{
		Logger: log, Audit: lg, Providers: map[string]core.ProviderConfig{"fr": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	res, err := d.Invoke(bg, core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "facts-tests", Scopes: []string{"fr:read"}},
		Surface: core.SurfaceREST, Op: factsRead.Op(), Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out, ok := res.Body.(factsOut); !ok || !out.Collected {
		t.Fatalf("body = %#v, want the handler to have run with a collector", res.Body)
	}
	page, err := lg.Query(audit.Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("audit records = %+v, want none for a read op", page.Items)
	}
}
