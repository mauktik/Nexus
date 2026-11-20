// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (nginx-provider)
//
// Stub of planning/nginx-provider/test-cases.md TC-55. The registry part of
// the body is complete: the developer of WI 4.3 deletes the t.Skip line. The
// OpenAPI enumeration part is asserted where the OpenAPI document is built
// (any golden that enumerates problem types). An un-skipped passing test is
// the GREEN signal.

package problem

import (
	"slices"
	"testing"
)

// nginx-provider TC-55: The four nginx problem types are registered.
// Owner WI 4.3. Requirements: FR-17.
func TestNginxProblemTypesRegistered(t *testing.T) {
	want := []struct {
		slug    Slug
		title   string
		status  int
		opLevel bool
		ext     []string
	}{
		{"nginx-config-invalid", "nginx configuration invalid", 422, false, []string{"testOutput", "rolledBack"}},
		{"nginx-config-unmanaged", "nginx configuration not managed", 422, false, []string{"reason", "fileName"}},
		{"nginx-not-running", "nginx not running", 422, false, []string{"state"}},
		{"nginx-reload-failed", "nginx reload failed", 0, true, []string{"observed", "rolledBack", "rollbackReload", "testOutput"}},
	}
	for _, w := range want {
		got, ok := Lookup(w.slug)
		if !ok {
			t.Errorf("%s is not registered", w.slug)
			continue
		}
		if got.Title != w.title {
			t.Errorf("%s title %q, want %q", w.slug, got.Title, w.title)
		}
		if got.Status != w.status || got.OperationLevel != w.opLevel {
			t.Errorf("%s status %d operationLevel %v, want %d %v", w.slug, got.Status, got.OperationLevel, w.status, w.opLevel)
		}
		if got.Exit != 1 {
			t.Errorf("%s exit %d, want 1", w.slug, got.Exit)
		}
		for _, e := range w.ext {
			if !slices.Contains(got.Extensions, e) {
				t.Errorf("%s lacks extension %q (has %v)", w.slug, e, got.Extensions)
			}
		}
		if _, ok := LookupURI(TypePrefix + string(w.slug)); !ok {
			t.Errorf("LookupURI does not resolve %s", w.slug)
		}
	}
}
