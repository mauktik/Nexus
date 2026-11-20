// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-28, TC-RA-29.
// Owner WIs: 2.2. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package conformance_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// TC-RA-28: The conformance word guard exempts exactly the three token op IDs. Owner WI 2.2. Requirements: FR-10, NFR-7.
//
//   - Setup: The real catalog, plus a planted extra spec with the op `tcra.tokens.list` and the route `/providers/tcra/tokens`.
//   - Steps:
//     1. Run the forbidden-word check over the catalog.
//     2. Run it over the catalog plus the planted spec.
//   - Expected:
//   - `allowedTokenOps` equals exactly the three nexusd token IDs.
//   - Step 1 passes.
//   - Step 2 fails, naming `tcra.tokens.list` and the word `tokens`.
//   - `TestTCSM101` passes unchanged: its pinned phrases are still in the `forbiddenWords` comment, and `create` is still forbidden in nexusd op IDs.
func TestTCRA28_TheConformanceWordGuardExemptsExactlyTheThree(t *testing.T) {
	want := []string{"nexusd.tokens.list", "nexusd.tokens.add", "nexusd.tokens.remove"}
	if !slices.Equal(allowedTokenOps, want) {
		t.Errorf("allowedTokenOps %q, want %q", allowedTokenOps, want)
	}

	// Step 1: the catalog, token ops included, has no forbidden word.
	var tokenOps []string
	for _, op := range catalog.Ops() {
		if slices.Contains(words(op.ID), "tokens") {
			tokenOps = append(tokenOps, op.ID)
		}
		for _, f := range forbiddenWordHits(t, op) {
			t.Errorf("catalog: %s", f)
		}
	}
	if !slices.Equal(tokenOps, want) {
		t.Errorf("catalog ops with the word tokens %q, want %q", tokenOps, want)
	}

	// Step 2: a planted token op outside the three is caught.
	planted := def.Define[nothing, nothing](def.Op{
		ID: "tcra.tokens.list", Provider: "tcra", Kind: def.KindList, ResourceType: "tokens",
		Summary: "Planted", Description: "A planted token op that the word guard must reject.",
		Scope: def.Scope{Name: "tcra:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "tcratoken", Verb: "get"},
	}).Op()
	if _, route, err := openapi.Route(planted); err != nil || route != "/v1/providers/tcra/tokens" {
		t.Fatalf("planted route %q, %v", route, err)
	}
	var planting []string
	for _, op := range append(catalog.Ops(), planted) {
		planting = append(planting, forbiddenWordHits(t, op)...)
	}
	if len(planting) == 0 || !slices.ContainsFunc(planting, func(f string) bool {
		return strings.Contains(f, "tcra.tokens.list") && strings.Contains(f, `"tokens"`)
	}) {
		t.Errorf("planted spec: findings %q, want one naming tcra.tokens.list and \"tokens\"", planting)
	}

	// TestTCSM101's guards stay: create is still forbidden in nexusd op IDs.
	createOp := planted
	createOp.ID, createOp.Provider = "nexusd.tokens.create", "nexusd"
	if f := forbiddenWordHits(t, createOp); !slices.ContainsFunc(f, func(f string) bool {
		return strings.Contains(f, `"create"`)
	}) {
		t.Errorf("nexusd.tokens.create: findings %q, want \"create\"", f)
	}
}

// TC-RA-29: Generic conformance treats SyncMutation and Audited ops by their declaration. Owner WI 2.2. Requirements: FR-13, CON-3, NFR-7.
//
//   - Setup: `catalogStubs` with `testprovider.Sync` entries for the three token ops.
//   - Steps:
//     1. Run the scope/audit expectations over the token ops.
//   - Expected:
//   - add and remove answer 200 with no `operation` member, and write `requested` then `completed`. They are never 202.
//   - list writes `requested` then `completed`.
//   - Each refuses a `nexusd:read`-only token with 403 and a `denied` record.
//   - `TestScopeAndAuditCoverEveryOp` is green over the full catalog.
func TestTCRA29_GenericConformanceTreatsSyncMutationAndAudited(t *testing.T) {
	s := newStack(t)
	admin, reader := s.exact[nexusd.AdminScope], s.exact[nexusd.ReadScope]
	for _, op := range []def.Op{nexusd.TokensList.Op(), nexusd.TokensAdd.Op(), nexusd.TokensRemove.Op()} {
		in := map[string]any{}
		if op.Kind == def.KindResourceAction {
			in["name"] = "ci"
		}
		s.assertSyncAudited(t, op, admin, s.exactName[nexusd.AdminScope], in)

		before := len(s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied,
			TokenName: s.exactName[nexusd.ReadScope]}))
		r := s.call(t, op, reader, in)
		if r.status != http.StatusForbidden {
			t.Errorf("%s with %s: %d %.200s, want 403", op.ID, nexusd.ReadScope, r.status, r.body)
		} else if p := r.problem(t); p.Extensions["requiredScope"] != nexusd.AdminScope {
			t.Errorf("%s: requiredScope %v, want %s", op.ID, p.Extensions["requiredScope"], nexusd.AdminScope)
		}
		after := s.records(t, audit.Query{Action: op.ID, Outcome: audit.OutcomeDenied,
			TokenName: s.exactName[nexusd.ReadScope]})
		if len(after) != before+1 {
			t.Errorf("%s: %d new denial records, want 1", op.ID, len(after)-before)
		}
	}
	// TestScopeAndAuditCoverEveryOp applies assertSyncAudited to every
	// SyncMutation and Audited op of the full catalog.
}
