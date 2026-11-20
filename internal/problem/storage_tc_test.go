// Tests of planning/storage-provider/test-cases.md TC-STG-18 (WI 1.3).

package problem

import (
	"encoding/json"
	"slices"
	"testing"
)

// TC-STG-18: the five storage problem types are registered with their status, exit code and extensions. Owner WI 1.3. Requirements: FR-13, FR-14, FR-19.
//
// not-allowed carries storage's extensions only; the union with the services
// branch's service and action is a merge-time change (M-1).
func TestTCSTG18_TheFiveStorageProblemTypesAreRegistered(t *testing.T) {
	want := []Type{
		// rc7 integration: not-allowed is shared with services, its extensions the union (M-1).
		{Slug: "not-allowed", Status: 409, Exit: 1, Extensions: []string{"service", "action", "configKey", "pool", "dataset"}},
		{Slug: "scan-in-progress", Status: 409, Exit: 1, Extensions: []string{"pool", "function", "state"}},
		{Slug: "pool-unhealthy", Status: 409, Exit: 1, Extensions: []string{"pool", "health"}},
		{Slug: "pool-low-space", Status: 409, Exit: 1, Extensions: []string{"pool", "capacityPercent", "maxCapacityPercent"}},
		{Slug: "zfs-command-failed", Exit: 1, OperationLevel: true, Extensions: []string{"command", "exitCode", "output"}},
	}
	consts := map[Slug]Slug{
		"not-allowed": NotAllowed, "scan-in-progress": ScanInProgress, "pool-unhealthy": PoolUnhealthy,
		"pool-low-space": PoolLowSpace, "zfs-command-failed": ZFSCommandFailed,
	}
	for _, w := range want {
		if consts[w.Slug] != w.Slug {
			t.Errorf("%s: constant is %q", w.Slug, consts[w.Slug])
		}
		got, ok := Lookup(w.Slug)
		if !ok {
			t.Errorf("%s: not registered", w.Slug)
			continue
		}
		if got.Title == "" || got.Status != w.Status || got.Exit != w.Exit || got.OperationLevel != w.OperationLevel {
			t.Errorf("%s: %+v, want status=%d exit=%d opLevel=%v and a title", w.Slug, got, w.Status, w.Exit, w.OperationLevel)
		}
		if !slices.Equal(got.Extensions, w.Extensions) {
			t.Errorf("%s: extensions %v, want %v", w.Slug, got.Extensions, w.Extensions)
			continue
		}
		if n := countSlug(w.Slug); n != 1 {
			t.Errorf("%s: registered %d times", w.Slug, n)
		}

		p := New(w.Slug, "detail")
		for i, k := range w.Extensions {
			p.With(k, i+1)
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("%s: marshal: %v", w.Slug, err)
		}
		var back Error
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", w.Slug, b, err)
		}
		if back.Slug() != w.Slug || back.Status != w.Status || back.Detail != "detail" || len(back.Extensions) != len(w.Extensions) {
			t.Errorf("%s: round trip %s -> %+v", w.Slug, b, back)
		}
		for i, k := range w.Extensions {
			if v, ok := back.Extensions[k].(float64); !ok || int(v) != i+1 {
				t.Errorf("%s: extension %s = %v after the round trip", w.Slug, k, back.Extensions[k])
			}
		}

		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: With(undeclared) did not panic", w.Slug)
				}
			}()
			// rc7 integration: service is declared on the shared not-allowed (M-1), so the probe key is one no type declares.
			New(w.Slug, "x").With("undeclaredKey", "x")
		}()
	}
}

func countSlug(slug Slug) int {
	n := 0
	for _, typ := range All() {
		if typ.Slug == slug {
			n++
		}
	}
	return n
}
