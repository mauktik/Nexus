// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-201, TC-STG-202, TC-STG-203, TC-STG-E1.
// Owner WIs: 5.1 (the storage stubs of catalogStubs and the FR-25 inputs of
// negative_test.go).

package conformance_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	storage "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// storageInputs are valid inputs of each storage op.
func storageInputs(op def.Op) map[string]any {
	switch op.ID {
	case storage.PoolsGet.Op().ID, storage.PoolsScrub.Op().ID, storage.PoolsStopScrub.Op().ID:
		return map[string]any{"name": "z1"}
	case storage.DatasetsGet.Op().ID:
		return map[string]any{"name": "z1/ds/appdata"}
	case storage.DatasetsSnapshot.Op().ID:
		return map[string]any{"name": "z1/ds/share"}
	}
	return map[string]any{}
}

// storageOps are the storage ops the stack serves, in catalog order.
func (s *stack) storageOps(t *testing.T) []def.Op {
	t.Helper()
	var out []def.Op
	for _, op := range s.ops() {
		if op.Provider == storage.Name {
			out = append(out, op)
		}
	}
	if len(out) != len(storage.Spec().Ops) {
		t.Fatalf("the stack serves %d storage ops, the spec declares %d", len(out), len(storage.Spec().Ops))
	}
	return out
}

// mintAllBut mints a token holding every scope of the stack but except.
func (s *stack) mintAllBut(t *testing.T, name, except string) string {
	t.Helper()
	var scopes []string
	for _, sc := range allScopes(s.reg) {
		if sc != except {
			scopes = append(scopes, sc)
		}
	}
	tok, _, err := auth.Create(s.stateDir, auth.CreateRequest{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatalf("token %s: %v", name, err)
	}
	return tok
}

// callsOf counts the spy's handler calls of storage ops.
func (s *stack) storageCalls() []testprovider.Call {
	var out []testprovider.Call
	for _, c := range s.spy.Calls() {
		if strings.HasPrefix(c.Op, storage.Name+".") {
			out = append(out, c)
		}
	}
	return out
}

// fieldErrors are the paths of a validation-failed answer's errors.
func fieldErrors(t *testing.T, r response) []problem.FieldError {
	t.Helper()
	var body struct {
		Errors []problem.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("status %d: %v\n%.300s", r.status, err, r.body)
	}
	return body.Errors
}

// TC-STG-201: conformance stubs, scope coverage and audit coverage pass for every storage op. Owner WI 5.1. Requirements: FR-2, FR-19, NFR-9.
//
// TestScopeAndAuditCoverEveryOp and TestAttackStringsAgainstEveryStringParameter
// cover the storage ops through catalogStubs; this checks each op against a
// token holding every scope but its own, and the audit records a read and a
// write leave.
func TestTCSTG201_ConformanceStubsScopeCoverageAndAuditCoverage(t *testing.T) {
	s := newStack(t)
	ops := s.storageOps(t)
	scopes := map[string]bool{}
	for _, op := range ops {
		scopes[op.Scope.Name] = true
	}
	if want := map[string]bool{storage.ReadScope: true, storage.ScrubScope: true, storage.SnapshotScope: true}; !maps.Equal(scopes, want) {
		t.Fatalf("storage ops name scopes %v, want %v", sortedKeys(scopes), sortedKeys(want))
	}
	allBut := map[string]string{}
	for sc := range scopes {
		allBut[sc] = s.mintAllBut(t, "all-but-"+strings.ReplaceAll(sc, ":", "-"), sc)
	}

	for _, op := range ops {
		before := len(s.storageCalls())
		r := s.call(t, op, allBut[op.Scope.Name], storageInputs(op))
		if r.status != http.StatusForbidden {
			t.Errorf("%s with every scope but %s: %d %.200s, want 403", op.ID, op.Scope.Name, r.status, r.body)
			continue
		}
		p := r.problem(t)
		if p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != op.Scope.Name {
			t.Errorf("%s: problem %s requiredScope %v, want forbidden %s", op.ID, p.Slug(), p.Extensions["requiredScope"], op.Scope.Name)
		}
		if n := len(s.storageCalls()) - before; n != 0 {
			t.Errorf("%s: refused, yet %d handler calls ran", op.ID, n)
		}
	}

	// The exact scope lets each op through: a read writes no audit record,
	// a write its requested record and a terminal one naming its operation.
	for _, op := range ops {
		tokName := s.exactName[op.Scope.Name]
		q := audit.Query{Action: op.ID, TokenName: tokName, Limit: 1000}
		before, err := s.audit.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		r := s.call(t, op, s.exact[op.Scope.Name], storageInputs(op))
		switch op.Mode {
		case def.ModeSync:
			if r.status != http.StatusOK {
				t.Errorf("%s: %d %.200s, want 200", op.ID, r.status, r.body)
				continue
			}
			after, err := s.audit.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(after.Items) - len(before.Items); n != 0 {
				t.Errorf("%s: a read wrote %d audit records, want 0", op.ID, n)
			}
		case def.ModeLongRunning:
			if r.status != http.StatusAccepted {
				t.Errorf("%s: %d %.200s, want 202", op.ID, r.status, r.body)
				continue
			}
			var o apitypes.Operation
			if err := json.Unmarshal(r.body, &o); err != nil {
				t.Fatal(err)
			}
			s.waitCall(t, func(c testprovider.Call) bool { return c.OperationID == o.ID })
			final := s.waitTerminal(t, o.ID)
			after, err := s.audit.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, rec := range before.Items {
				seen[rec.ID] = true
			}
			var outcomes []string
			for _, rec := range after.Items {
				if !seen[rec.ID] {
					outcomes = append(outcomes, rec.Outcome)
				}
			}
			// The operation's state records sit between the two.
			if !slices.Contains(outcomes, audit.OutcomeRequested) || !slices.Contains(outcomes, string(final.Status)) ||
				!final.Status.Terminal() || slices.Contains(outcomes, audit.OutcomeDenied) ||
				slices.Contains(outcomes, audit.OutcomeRefused) {
				t.Errorf("%s: new audit outcomes %v, want requested and the terminal %s", op.ID, outcomes, final.Status)
			}
		default:
			t.Errorf("%s: mode %s", op.ID, op.Mode)
		}
	}

	// A storage:read token cannot scrub; a storage:scrub token cannot
	// snapshot.
	for _, c := range []struct {
		scope string
		op    def.Op
	}{{storage.ReadScope, storage.PoolsScrub.Op()}, {storage.ScrubScope, storage.DatasetsSnapshot.Op()}} {
		r := s.call(t, c.op, s.exact[c.scope], storageInputs(c.op))
		if r.status != http.StatusForbidden || r.problem(t).Extensions["requiredScope"] != c.op.Scope.Name {
			t.Errorf("%s with only %s: %d %.200s, want 403 naming %s", c.op.ID, c.scope, r.status, r.body, c.op.Scope.Name)
		}
	}
}

// TC-STG-202: FR-25 negative inputs are refused with 400 before any handler call. Owner WI 5.1. Requirements: NFR-5, FR-5, FR-7, FR-14.
func TestTCSTG202_FR25NegativeInputsAreRefusedWith(t *testing.T) {
	s := newStack(t)
	cases := storageNegatives()
	fields := map[string]bool{}
	for _, c := range cases {
		fields[c.field] = true
	}
	if want := []string{"cursor", "dataset", "label", "name", "pool", "snapshot", "vmid"}; !slices.Equal(sortedKeys(fields), want) {
		t.Fatalf("storageNegatives covers %v, want %v", sortedKeys(fields), want)
	}
	// The control: each op's valid input reaches its stub.
	for _, op := range s.storageOps(t) {
		r := s.call(t, op, s.full, storageInputs(op))
		if r.status/100 != 2 {
			t.Fatalf("control %s: %d %.300s", op.ID, r.status, r.body)
		}
		if r.status == http.StatusAccepted {
			s.received(t, op, r, 0) // Its run must not land among a refusal's calls
		}
	}
	for _, c := range cases {
		v := fmt.Sprint(c.input[c.field])
		t.Run(fmt.Sprintf("%s/%s=%.40s", c.op.ID, c.field, v), func(t *testing.T) {
			before := len(s.storageCalls())
			r := s.call(t, c.op, s.full, c.input)
			if r.status != http.StatusBadRequest {
				t.Fatalf("%d %.300s, want 400", r.status, r.body)
			}
			if p := r.problem(t); p.Slug() != problem.ValidationFailed {
				t.Errorf("problem %s, want validation-failed", p.Slug())
			}
			errs := fieldErrors(t, r)
			if !slices.ContainsFunc(errs, func(e problem.FieldError) bool {
				return strings.Contains(e.Path, c.field) || strings.Contains(e.Message, c.field)
			}) {
				t.Errorf("errors %+v do not name %s", errs, c.field)
			}
			if n := len(s.storageCalls()) - before; n != 0 {
				t.Errorf("refused, yet %d handler calls ran", n)
			}
		})
	}
}

// TC-STG-203: a dataset name with slashes round-trips through the real REST routes. Owner WI 5.1. Requirements: FR-7, FR-14, FR-17.
func TestTCSTG203_ADatasetNameWithSlashesRoundTrips(t *testing.T) {
	s := newStack(t)
	const base = "/v1/providers/storage/datasets/"
	nameOf := func(t *testing.T, c testprovider.Call) string {
		t.Helper()
		in, err := testprovider.InputJSON(c)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := in.(map[string]any)["name"].(string)
		return name
	}

	// The escaped GET reaches storage.datasets.get with the whole name.
	before := len(s.storageCalls())
	r := s.do(t, http.MethodGet, base+"z1%2Fds%2Fappdata", s.full, nil)
	calls := s.storageCalls()[before:]
	if r.status != http.StatusOK || len(calls) != 1 || calls[0].Op != storage.DatasetsGet.Op().ID {
		t.Fatalf("GET z1%%2Fds%%2Fappdata: %d %.200s, calls %+v, want 200 and one storage.datasets.get", r.status, r.body, calls)
	}
	if got := nameOf(t, calls[0]); got != "z1/ds/appdata" {
		t.Errorf("storage.datasets.get received name %q, want z1/ds/appdata", got)
	}

	// The escaped POST reaches storage.datasets.snapshot with the whole name.
	r = s.do(t, http.MethodPost, base+"z1%2Fds%2Fshare/snapshot", s.full, []byte(`{}`))
	if r.status != http.StatusAccepted {
		t.Fatalf("POST z1%%2Fds%%2Fshare/snapshot: %d %.200s, want 202", r.status, r.body)
	}
	var o apitypes.Operation
	if err := json.Unmarshal(r.body, &o); err != nil {
		t.Fatal(err)
	}
	c := s.waitCall(t, func(c testprovider.Call) bool { return c.OperationID == o.ID })
	if c.Op != storage.DatasetsSnapshot.Op().ID || nameOf(t, c) != "z1/ds/share" {
		t.Errorf("snapshot call %s name %q, want storage.datasets.snapshot of z1/ds/share", c.Op, nameOf(t, c))
	}
	s.waitTerminal(t, o.ID)

	// The traversal is refused by the schema; the unescaped form has no
	// route. Neither reaches a handler.
	for _, tc := range []struct {
		path   string
		status int
		slug   problem.Slug
	}{
		{base + "z1%2F..%2Fetc", http.StatusBadRequest, problem.ValidationFailed},
		{base + "z1/ds/appdata", http.StatusNotFound, ""},
	} {
		before := len(s.storageCalls())
		r := s.do(t, http.MethodGet, tc.path, s.full, nil)
		if r.status != tc.status {
			t.Errorf("GET %s: %d %.200s, want %d", tc.path, r.status, r.body, tc.status)
		} else if tc.slug != "" && r.problem(t).Slug() != tc.slug {
			t.Errorf("GET %s: problem %s, want %s", tc.path, r.problem(t).Slug(), tc.slug)
		}
		if got := s.storageCalls()[before:]; len(got) != 0 {
			t.Errorf("GET %s: handler calls %+v, want none", tc.path, got)
		}
	}
}

// TC-STG-E1: dataset names at 255 bytes are accepted end to end and 256 are refused. Owner WI 5.1. Requirements: FR-7, NFR-5.
func TestTCSTGE1_DatasetNamesAt255BytesAreAccepted(t *testing.T) {
	s := newStack(t)
	for _, n := range []int{storage.MaxNameLength, storage.MaxNameLength + 1} {
		name := "z1/ds/" + strings.Repeat("a", n-len("z1/ds/"))
		if len(name) != n {
			t.Fatalf("name of %d bytes, want %d", len(name), n)
		}
		before := len(s.storageCalls())
		r := s.do(t, http.MethodGet, "/v1/providers/storage/datasets/"+url.PathEscape(name), s.full, nil)
		calls := s.storageCalls()[before:]
		if n == storage.MaxNameLength {
			if r.status != http.StatusOK || len(calls) != 1 {
				t.Fatalf("%d bytes: %d %.200s, calls %d, want 200 and one call", n, r.status, r.body, len(calls))
			}
			in, err := testprovider.InputJSON(calls[0])
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := in.(map[string]any)["name"].(string); got != name {
				t.Errorf("%d bytes: the stub received %d bytes, want the exact name", n, len(got))
			}
			continue
		}
		if r.status != http.StatusBadRequest || r.problem(t).Slug() != problem.ValidationFailed {
			t.Errorf("%d bytes: %d %.200s, want 400 validation-failed", n, r.status, r.body)
		}
		if len(calls) != 0 {
			t.Errorf("%d bytes: %d handler calls, want none", n, len(calls))
		}
	}
}
