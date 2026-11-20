// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-121, TC-SV-122.
// Owner WI: 5.1. The services ops run against their stubs in catalogStubs.

package conformance_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	services "github.com/mauktik/nexus/internal/providers/services/spec"
)

// servicesOps returns the stack's services ops, failing unless they are
// exactly the spec's eight.
func servicesOps(t *testing.T, s *stack) []def.Op {
	t.Helper()
	var out, want []string
	var ops []def.Op
	for _, op := range s.ops() {
		if op.Provider == services.Name {
			ops = append(ops, op)
			out = append(out, op.ID)
		}
	}
	for _, op := range services.Spec().Ops {
		want = append(want, op.ID)
	}
	slices.Sort(out)
	slices.Sort(want)
	if len(want) != 8 || !slices.Equal(out, want) {
		t.Fatalf("the stack binds services ops %v, want the eight %v", out, want)
	}
	return ops
}

// TC-SV-121: Conformance covers every services op with scopes, audit and the FR-25 negative inputs. Owner WI 5.1. Requirements: FR-2, FR-15, NFR-5, NFR-8.
//
// - Setup: `catalogStubs` with the services stubs (the reboot stub declares `AfterReboot`).
// - Steps:
//  1. Run `TestScopeAndAuditCoverEveryOp`, `TestEveryStringInputIsConstrained` and `TestAttackStringsAgainstEveryStringParameter` restricted to `services.*`.
//  2. For every op with `name`, send `../x`, `a/b`, `a;b`, `a b`, a 65-character name, `-x` and `x.service`.
//  3. For the reboot, send `confirmHost` values `h1\n`, `h1\x00` and `h1\x1b[31m`.
//
// - Expected: Every listed input answers 400 `validation-failed` naming the field, with no handler called (the stub records zero calls). Every services op appears in the scope and audit coverage.
func TestTCSV121_ConformanceCoversEveryServicesOpWithScopes(t *testing.T) {
	s := newStack(t)
	ops := servicesOps(t, s)
	sm := sampler{target: s.restart(t, s.full).ID}

	// Step 1: the generic walks reach every services op. The string walks
	// are the full tests; here, the services ops' share of them.
	for _, op := range ops {
		if f := unconstrained(t, op); len(f) != 0 {
			t.Errorf("unconstrained input strings: %v", f)
		}
		if f := executionSurface(t, op); len(f) != 0 {
			t.Errorf("execution surface: %v", f)
		}
		need := op.Scope.Name
		before := len(s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied, TokenName: noneToken}))
		base, _ := sm.value(t, op, "", inputSchema(t, op)).(map[string]any)
		if r := s.call(t, op, s.none, base); r.status != http.StatusForbidden {
			t.Errorf("%s without a scope: %d %.200s, want 403", op.ID, r.status, r.body)
		} else if got := r.problem(t).Extensions["requiredScope"]; got != need {
			t.Errorf("%s: requiredScope %v, want %s", op.ID, got, need)
		}
		after := s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied, TokenName: noneToken})
		if len(after) != before+1 {
			t.Errorf("%s: %d new denial records, want 1", op.ID, len(after)-before)
		}
	}

	// Steps 2 and 3: each FR-25 negative answers 400 validation-failed
	// naming the field, before any handler.
	checked := 0
	for _, op := range ops {
		base, _ := sm.value(t, op, "", inputSchema(t, op)).(map[string]any)
		for _, path := range stringParams(t, op) {
			for _, a := range servicesAttacks[path] {
				t.Run(fmt.Sprintf("%s%s/%s", op.ID, path, a.name), func(t *testing.T) {
					in := sm.withValue(t, op, base, path, a.value)
					before := len(s.spy.Calls())
					r := s.call(t, op, s.full, in)
					if r.status != http.StatusBadRequest {
						t.Fatalf("%q: %d %.300s, want 400", a.value, r.status, r.body)
					}
					p := r.problem(t)
					if p.Slug() != problem.ValidationFailed {
						t.Errorf("400 with problem %s", p.Slug())
					}
					if b, _ := json.Marshal(p.Extensions["errors"]); !strings.Contains(string(b), strings.TrimPrefix(path, "/")) {
						t.Errorf("the problem's errors %s do not name %s", b, path)
					}
					if n := len(s.spy.Calls()); n != before {
						t.Errorf("rejected, yet %d handler calls ran", n-before)
					}
				})
				checked++
			}
		}
	}
	// get, logs, start, stop, restart and control take name (6 x 7), and
	// the reboot takes confirmHost (3).
	if checked != 6*7+3 {
		t.Errorf("sent %d negatives, want %d", checked, 6*7+3)
	}
}

// TC-SV-122: Each op refuses a token without its scope with 403 naming the scope. Owner WI 5.1. Requirements: FR-2.
//
// - Setup: Tokens: `read` (`services:read`), `logs` (`services:logs`), `write` (`services:write`) and `reboot` (`services:reboot`).
// - Steps:
//  1. Call every op with every token.
//
// - Expected:
//   - each op succeeds or passes scope with exactly its own scope;
//   - with any other token it answers 403 `forbidden`, with `requiredScope` = its scope;
//   - in particular, `read` cannot get logs, `write` cannot reboot, and `reboot` cannot start.
func TestTCSV122_EachOpRefusesATokenWithoutIts(t *testing.T) {
	s := newStack(t)
	ops := servicesOps(t, s)
	sm := sampler{target: s.restart(t, s.full).ID}
	scopes := []string{services.ReadScope, services.LogsScope, services.WriteScope, services.RebootScope}
	want := map[string]string{
		services.List.Op().ID: services.ReadScope, services.Get.Op().ID: services.ReadScope,
		services.Logs.Op().ID:  services.LogsScope,
		services.Start.Op().ID: services.WriteScope, services.Stop.Op().ID: services.WriteScope,
		services.Restart.Op().ID: services.WriteScope, services.Control.Op().ID: services.WriteScope,
		services.Reboot.Op().ID: services.RebootScope,
	}
	for _, op := range ops {
		if op.Scope.Name != want[op.ID] {
			t.Errorf("%s declares scope %q, want %q", op.ID, op.Scope.Name, want[op.ID])
		}
		in, _ := sm.value(t, op, "", inputSchema(t, op)).(map[string]any)
		for _, sc := range scopes {
			tok := s.exact[sc]
			if tok == "" {
				t.Fatalf("no token holding exactly %s", sc)
			}
			r := s.call(t, op, tok, in)
			if sc == want[op.ID] {
				if r.status == http.StatusForbidden || r.status/100 != 2 {
					t.Errorf("%s with exactly %s: %d %.200s, want success", op.ID, sc, r.status, r.body)
				}
				continue
			}
			if r.status != http.StatusForbidden {
				t.Errorf("%s with %s: %d %.200s, want 403", op.ID, sc, r.status, r.body)
				continue
			}
			p := r.problem(t)
			if p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != want[op.ID] {
				t.Errorf("%s with %s: %s requiredScope %v, want forbidden naming %s", op.ID, sc, p.Slug(),
					p.Extensions["requiredScope"], want[op.ID])
			}
		}
	}
}
