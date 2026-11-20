// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: RED-by-design (WI 1.2). Compiles today and fails its
// assertions until the nine pihole slugs are in the registry; GREEN when
// WI 1.2 lands. The developer may fold it into problem_test.go.
// Case: TC-2 in planning/pihole-dns-zones/test-cases.md; expected rows are
// the "New problem types" table of architecture.md.
package problem

import (
	"encoding/json"
	"slices"
	"testing"
)

// TC-2: the nine pihole problem types are registered exactly as tabled, and
// With accepts each declared extension and refuses any other.
func TestTC2_PiholeProblemTypes(t *testing.T) {
	want := []Type{
		{Slug: "dns-file-conflict", Status: 409, Exit: 1, Extensions: []string{"expectedEtag", "currentEtag"}},
		{Slug: "dns-name-unzoned", Status: 422, Exit: 1, Extensions: []string{"name"}},
		{Slug: "dns-batch-rejected", Status: 422, Exit: 1, Extensions: []string{"changeIndex", "cause"}},
		{Slug: "dns-config-test-failed", Exit: 1, OperationLevel: true, Extensions: []string{"exitCode", "rolledBack", "preExisting"}},
		{Slug: "dns-verification-failed", Exit: 1, OperationLevel: true, Extensions: []string{"rolledBack", "name", "expected", "observed"}},
		{Slug: "dns-rollback-failed", Exit: 1, OperationLevel: true, Extensions: []string{"rollbackCopy"}},
		{Slug: "gravity-failed", Exit: 1, OperationLevel: true, Extensions: []string{"exitCode"}},
		{Slug: "pihole-command-failed", Exit: 1, OperationLevel: true, Extensions: []string{"command", "exitCode", "rolledBack"}},
		{Slug: "pihole-busy", Exit: 1, OperationLevel: true, Extensions: []string{"waitedSeconds"}},
	}
	for _, w := range want {
		got, ok := Lookup(w.Slug)
		if !ok {
			t.Errorf("%s: not registered", w.Slug)
			continue
		}
		if got.Title == "" || got.Status != w.Status || got.Exit != w.Exit || got.OperationLevel != w.OperationLevel {
			t.Errorf("%s: %+v, want status=%d exit=%d opLevel=%v and a title", w.Slug, got, w.Status, w.Exit, w.OperationLevel)
		}
		a, b := slices.Clone(got.Extensions), slices.Clone(w.Extensions)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Errorf("%s: extensions %v, want %v", w.Slug, got.Extensions, w.Extensions)
			continue
		}
		p := New(w.Slug, "x")
		for _, k := range w.Extensions {
			p.With(k, 1)
		}
		if body, err := json.Marshal(p); err != nil || len(body) == 0 {
			t.Errorf("%s: marshal %v", w.Slug, err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: With(undeclared) did not panic", w.Slug)
				}
			}()
			New(w.Slug, "x").With("password", "x")
		}()
	}
}
