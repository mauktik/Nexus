// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package conformance_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	nginx "github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// nginx-provider TC-58: the conformance suite covers every nginx op. The
// generic tests (TestScopeAndAuditCoverEveryOp, TestEveryStringInputIsConstrained,
// TestNoArbitraryExecutionSurface, TestAttackStringsAgainstEveryStringParameter)
// run over the nginx stub too; this test pins what is specific to nginx.
func TestNginxStubBindsEveryOp(t *testing.T) {
	// The stub binds exactly the 13 IDs: 4 sync reads, 9 stand-ins for the
	// detached ops.
	stub := catalogStubs[nginx.Name](&testprovider.Spy{})
	var bound, want []string
	kinds := map[provider.BindingKind]int{}
	for _, b := range stub.Bindings() {
		bound = append(bound, b.Op().ID)
		kinds[b.Kind()]++
		if (b.Kind() == provider.BindSync) != (b.Op().Mode == def.ModeSync) {
			t.Errorf("%s: bound %s for mode %s", b.Op().ID, b.Kind(), b.Op().Mode)
		}
	}
	for _, op := range nginx.Spec().Ops {
		want = append(want, op.ID)
	}
	if !slices.Equal(bound, want) || len(bound) != 13 {
		t.Errorf("stub binds %v, want the 13 %v", bound, want)
	}
	if kinds[provider.BindSync] != 4 || kinds[provider.BindInProcess] != 9 {
		t.Errorf("binding kinds %v, want 4 sync and 9 in-process", kinds)
	}

	s := newStack(t)
	byID := map[string]def.Op{}
	for _, op := range s.ops() {
		if op.Provider == nginx.Name {
			byID[op.ID] = op
		}
	}
	if len(byID) != 13 {
		t.Fatalf("the stack serves %d nginx ops, want 13", len(byID))
	}

	// Scopes: reads need nginx:read, lifecycle and toggles nginx:write, add
	// and update nginx:config.
	scopes := map[string]string{
		"nginx.service.list": nginx.ReadScope, "nginx.actions.test": nginx.ReadScope,
		"nginx.sites.list": nginx.ReadScope, "nginx.sites.get": nginx.ReadScope, "nginx.sites.content": nginx.ReadScope,
		"nginx.actions.start": nginx.WriteScope, "nginx.actions.stop": nginx.WriteScope,
		"nginx.actions.restart": nginx.WriteScope, "nginx.actions.reload": nginx.WriteScope,
		"nginx.sites.enable": nginx.WriteScope, "nginx.sites.disable": nginx.WriteScope,
		"nginx.sites.add": nginx.ConfigScope, "nginx.sites.update": nginx.ConfigScope,
	}
	for id, want := range scopes {
		if got := byID[id].Scope.Name; got != want {
			t.Errorf("%s: scope %q, want %q", id, got, want)
		}
	}

	// A token holding only nginx:config cannot read the status.
	cfgTok := s.exact[nginx.ConfigScope]
	r := s.call(t, byID["nginx.service.list"], cfgTok, map[string]any{})
	if r.status != http.StatusForbidden {
		t.Fatalf("status with only nginx:config: %d %s", r.status, r.body)
	}
	if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != nginx.ReadScope {
		t.Errorf("problem %s %v, want forbidden naming %s", p.Slug(), p.Extensions, nginx.ReadScope)
	}

	// Every mutation is audited before it acts, with content redacted.
	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const secret = "server { listen 80; } # audit-must-not-see-this"
	for _, op := range byID {
		if !op.Mutating() {
			continue
		}
		in := map[string]any{}
		if strings.HasPrefix(op.ID, "nginx.sites.") {
			in["name"] = "example.com"
		}
		if op.Scope.Name == nginx.ConfigScope {
			in["content"] = secret
		}
		r := s.call(t, op, s.exact[op.Scope.Name], in)
		if r.status != http.StatusAccepted {
			t.Errorf("%s: %d %.300s, want 202", op.ID, r.status, r.body)
			continue
		}
		s.received(t, op, r, 0)
		page, err := s.audit.Query(audit.Query{Action: op.ID, Outcome: audit.OutcomeRequested, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Errorf("%s: %d requested records, want 1", op.ID, len(page.Items))
			continue
		}
		b, err := json.Marshal(page.Items[0])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "audit-must-not-see-this") {
			t.Errorf("%s: the audit record holds the content: %s", op.ID, b)
		}
	}

	// FR-25 negative inputs: name, ifMatch and cursor refuse every attack
	// with validation-failed before a handler runs; content accepts it as
	// free text and the handler receives it as one input field, byte for
	// byte, never as an argument.
	update, list := byID["nginx.sites.update"], byID["nginx.sites.list"]
	for _, a := range attacks {
		for _, c := range []struct {
			op def.Op
			in map[string]any
		}{
			{update, map[string]any{"name": a.value, "content": "x"}},
			{update, map[string]any{"name": "example.com", "content": "x", "ifMatch": a.value}},
			{list, map[string]any{"cursor": a.value}},
		} {
			before := len(s.spy.Calls())
			r := s.call(t, c.op, s.full, c.in)
			if r.status != http.StatusBadRequest {
				t.Errorf("%s %v with %s: %d %.200s, want 400", c.op.ID, keys(c.in), a.name, r.status, r.body)
				continue
			}
			if p := r.problem(t); p.Slug() != problem.ValidationFailed {
				t.Errorf("%s with %s: problem %s", c.op.ID, a.name, p.Slug())
			}
			if n := len(s.spy.Calls()) - before; n != 0 {
				t.Errorf("%s with %s: %d handler calls", c.op.ID, a.name, n)
			}
		}

		r := s.call(t, update, s.full, map[string]any{"name": "example.com", "content": a.value, "ifMatch": hex64})
		if r.status != http.StatusAccepted {
			t.Errorf("content %s: %d %.200s, want 202", a.name, r.status, r.body)
			continue
		}
		var o apitypes.Operation
		if err := json.Unmarshal(r.body, &o); err != nil {
			t.Fatal(err)
		}
		got := s.waitCall(t, func(c testprovider.Call) bool { return c.OperationID == o.ID })
		in, ok := got.Input.(nginx.WriteInput)
		if !ok {
			t.Fatalf("handler input is %T, want nginx.WriteInput", got.Input)
		}
		if in.Content != a.value || in.Name != "example.com" || in.IfMatch != hex64 {
			t.Errorf("content %s: handler received %.80q", a.name, in.Content)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
